// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: 2023-Present The UDS Authors

// Package bundler defines behavior for bundling packages
package bundler

import (
	"fmt"

	"github.com/defenseunicorns/uds-cli/src/config"
	"github.com/defenseunicorns/uds-cli/src/pkg/bundler/pusher"
	"github.com/defenseunicorns/uds-cli/src/pkg/utils"
	"github.com/defenseunicorns/uds-cli/src/types"
	"github.com/defenseunicorns/zarf/src/pkg/message"
	"github.com/defenseunicorns/zarf/src/pkg/oci"
	goyaml "github.com/goccy/go-yaml"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

type RemoteBundleOpts struct {
	Bundle    *types.UDSBundle
	TmpDstDir string
	Output    string
}

type RemoteBundle struct {
	bundle    *types.UDSBundle
	tmpDstDir string
	output    string
}

func NewRemoteBundle(opts *RemoteBundleOpts) *RemoteBundle {
	return &RemoteBundle{
		bundle:    opts.Bundle,
		tmpDstDir: opts.TmpDstDir,
		output:    opts.Output,
	}
}

// create creates the bundle in a remote OCI registry publishes w/ optional signature to the remote repository.
func (r *RemoteBundle) create(signature []byte) error {
	// set the bundle remote's reference from metadata
	r.output = utils.EnsureOCIPrefix(r.output)
	ref, err := referenceFromMetadata(r.output, &r.bundle.Metadata)
	if err != nil {
		return err
	}
	platform := ocispec.Platform{
		Architecture: config.GetArch(),
		OS:           oci.MultiOS,
	}

	// create the bundle remote
	bundleRemote, err := oci.NewOrasRemote(ref, platform)
	if err != nil {
		return err
	}
	bundle := r.bundle
	if bundle.Metadata.Architecture == "" {
		return fmt.Errorf("architecture is required for bundling")
	}
	dstRef := bundleRemote.Repo().Reference
	message.Debug("Bundling", bundle.Metadata.Name, "to", dstRef)

	rootManifest := ocispec.Manifest{}
	pusherConfig := pusher.Config{
		Bundle:    bundle,
		RemoteDst: bundleRemote,
		NumPkgs:   len(bundle.Packages),
	}

	for i, pkg := range bundle.Packages {
		// todo: can leave this block here or move to pusher.NewPkgPusher (would be closer to NewPkgFetcher pattern)
		pkgUrl := fmt.Sprintf("%s:%s", pkg.Repository, pkg.Ref)
		src, err := oci.NewOrasRemote(pkgUrl, platform)
		if err != nil {
			return err
		}
		pusherConfig.RemoteSrc = src
		pkgRootManifest, err := src.FetchRoot()
		if err != nil {
			return err
		}
		pusherConfig.PkgRootManifest = pkgRootManifest
		pusherConfig.PkgIter = i

		remotePusher := pusher.NewPkgPusher(pkg, pusherConfig)
		zarfManifestDesc, err := remotePusher.Push()
		if err != nil {
			return err
		}
		rootManifest.Layers = append(rootManifest.Layers, zarfManifestDesc)
	}

	// push the bundle's metadata
	bundleYamlBytes, err := goyaml.Marshal(bundle)
	if err != nil {
		return err
	}
	bundleYamlDesc, err := bundleRemote.PushLayer(bundleYamlBytes, oci.ZarfLayerMediaTypeBlob)
	if err != nil {
		return err
	}
	bundleYamlDesc.Annotations = map[string]string{
		ocispec.AnnotationTitle: config.BundleYAML,
	}

	message.Debug("Pushed", config.BundleYAML+":", message.JSONValue(bundleYamlDesc))
	rootManifest.Layers = append(rootManifest.Layers, bundleYamlDesc)

	// push the bundle's signature
	if len(signature) > 0 {
		bundleYamlSigDesc, err := bundleRemote.PushLayer(signature, oci.ZarfLayerMediaTypeBlob)
		if err != nil {
			return err
		}
		bundleYamlSigDesc.Annotations = map[string]string{
			ocispec.AnnotationTitle: config.BundleYAMLSignature,
		}
		rootManifest.Layers = append(rootManifest.Layers, bundleYamlSigDesc)
		message.Debug("Pushed", config.BundleYAMLSignature+":", message.JSONValue(bundleYamlSigDesc))
	}

	// push the bundle manifest config
	configDesc, err := pushManifestConfigFromMetadata(bundleRemote, &bundle.Metadata, &bundle.Build)
	if err != nil {
		return err
	}

	message.Debug("Pushed config:", message.JSONValue(configDesc))

	// check for existing index
	index, err := utils.GetIndex(bundleRemote, dstRef.String())
	if err != nil {
		return err
	}

	// push bundle root manifest
	rootManifest.Config = configDesc
	rootManifest.SchemaVersion = 2
	rootManifest.Annotations = manifestAnnotationsFromMetadata(&bundle.Metadata) // maps to registry UI
	rootManifestDesc, err := utils.ToOCIRemote(rootManifest, ocispec.MediaTypeImageManifest, bundleRemote)
	if err != nil {
		return err
	}

	// create or update, then push index.json
	err = utils.UpdateIndex(index, bundleRemote, bundle, rootManifestDesc)
	if err != nil {
		return err
	}

	message.HorizontalRule()
	flags := ""
	if config.CommonOptions.Insecure {
		flags = "--insecure"
	}
	message.Title("To inspect/deploy/pull:", "")
	message.Command("inspect oci://%s %s", dstRef, flags)
	message.Command("deploy oci://%s %s", dstRef, flags)
	message.Command("pull oci://%s %s", dstRef, flags)

	return nil
}