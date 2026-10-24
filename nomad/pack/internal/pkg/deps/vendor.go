// Copyright IBM Corp. 2021, 2025
// SPDX-License-Identifier: MPL-2.0

package deps

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	gg "github.com/hashicorp/go-getter"
	"github.com/hashicorp/hcl/v2/hclsimple"

	"github.com/hashicorp/nomad/pack/internal/pkg/oci"
	"github.com/hashicorp/nomad/pack/internal/pkg/packconfig"
	"github.com/hashicorp/nomad/pack/internal/pkg/packlock"
	"github.com/hashicorp/nomad/pack/internal/pkg/supplychain"
	"github.com/hashicorp/nomad/pack/sdk/pack"
	"github.com/hashicorp/nomad/pack/terminal"
)

// Options control vendoring.
type Options struct {
	// UpdateLock accepts dependencies whose content differs from the digest
	// recorded in pack.lock for the same source and ref. Without it such a
	// difference is an error, since the same version of a dependency should
	// never change.
	UpdateLock bool
}

// Vendor reads the metadata.hcl from the provided directory and downloads
// dependencies
func Vendor(ctx context.Context, ui terminal.UI, targetPath string) error {
	return VendorWithOptions(ctx, ui, targetPath, Options{})
}

// VendorWithOptions vendors the dependencies of the pack in targetPath into
// its deps/ directory and writes pack.lock with the digests of the pack and
// of every vendored dependency. Dependencies with an oci:// source are
// downloaded from the OCI registry and verified with the trust policy of the
// client configuration.
func VendorWithOptions(ctx context.Context, ui terminal.UI, targetPath string, opts Options) error {
	// attempt to read metadata.hcl
	metadata := &pack.Metadata{}
	err := hclsimple.DecodeFile(filepath.Join(targetPath, "metadata.hcl"), nil, metadata)
	if err != nil {
		return err
	}

	if len(metadata.Dependencies) == 0 {
		return errors.New("metadata.hcl file does not contain any dependencies")
	}

	prevLock, err := packlock.Read(targetPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}

	var conf *packconfig.Config
	manifestDigests := map[string]string{}
	for _, d := range metadata.Dependencies {
		targetDir := filepath.Join(targetPath, "deps", d.Name)

		if oci.IsOCI(d.Source) {
			if conf == nil {
				if conf, err = packconfig.LoadDefault(); err != nil {
					return err
				}
			}
			loc, err := oci.ParseLocation(d.Source)
			if err != nil {
				return fmt.Errorf("dependency %q: %w", d.Name, err)
			}
			ui.Info(fmt.Sprintf("downloading %v pack to %v...", d.Name, targetDir))
			res, err := supplychain.Unpack(ctx, supplychain.EndpointFor(conf, loc), d.Ref, targetDir)
			if err != nil {
				return fmt.Errorf("error downloading dependency %q: %w", d.Name, err)
			}
			manifestDigests[d.Name] = res.Digest
			if id := res.SignerKeyID(); id != "" {
				ui.Info(fmt.Sprintf("signature of %s verified with key %s", res.Digest, id))
			}
			ui.Success("...success!")
			continue
		}

		url := d.Source

		if !d.IsLatest() {
			url = fmt.Sprintf("%s?ref=%s", url, d.Ref)
		} else {
			// Attempt to shallow clone the constructed url
			url = fmt.Sprintf("%s?depth=1", url)
		}

		// download each dependency
		ui.Info(fmt.Sprintf("downloading %v pack to %v...", d.Name, targetDir))
		if err := gg.Get(targetDir, url, gg.WithContext(ctx)); err != nil {
			return fmt.Errorf("error downloading dependency: %v", err)
		}
		ui.Success("...success!")
	}

	return writeLock(ui, targetPath, metadata, prevLock, manifestDigests, opts)
}

// writeLock records the digests of the pack and its vendored dependencies in
// pack.lock. A dependency whose source and ref are unchanged must still have
// the locked digest, unless the update of the lock was requested.
func writeLock(ui terminal.UI, dir string, metadata *pack.Metadata, prev *packlock.Lock, manifestDigests map[string]string, opts Options) error {
	lock, err := packlock.Generate(dir, metadata, prev)
	if err != nil {
		return err
	}
	for _, d := range lock.Dependencies {
		if md, ok := manifestDigests[d.Name]; ok {
			d.ManifestDigest = md
		}
		if prev == nil || opts.UpdateLock {
			continue
		}
		old := prev.Dependency(d.Name)
		if old != nil && old.Source == d.Source && old.Ref == d.Ref && old.Digest != d.Digest {
			return fmt.Errorf("dependency %q (%s@%s) has digest %s, but %s records %s; "+
				"use --update-lock to accept the new content", d.Name, d.Source, d.Ref, d.Digest, packlock.FileName, old.Digest)
		}
	}
	if err := lock.Write(dir); err != nil {
		return err
	}
	ui.Info(fmt.Sprintf("wrote %s", packlock.Path(dir)))
	return nil
}
