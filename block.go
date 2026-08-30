package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// blockConfigName is the per-project policy file. vxv searches the working
// directory and every ancestor up to the filesystem root for it, so a file
// dropped anywhere above the launch point applies to sessions started beneath
// it. Every file found is merged (nearer directories do not override farther
// ones — the union of all lists is enforced).
const blockConfigName = ".vxv.yaml"

// blockConfig is the on-disk schema. Two independent controls, each a list of
// files, with aliases:
//
//	hide     (deny, block, files) — guest can neither read nor write the file
//	readonly (no-write, protect)  — guest can read the file but not write it
//
// A bare top-level YAML sequence is shorthand for `hide`. Relative entries are
// resolved against the directory holding the .vxv.yaml that named them.
//
//	# .vxv.yaml
//	hide:
//	  - secrets.env          # relative to this file's directory
//	  - /etc/shadow          # absolute host path
//	readonly:
//	  - config.yaml          # readable inside the guest, but immutable
type blockConfig struct {
	// hide: no read, no write.
	Hide  []string `yaml:"hide"`
	Deny  []string `yaml:"deny"`
	Block []string `yaml:"block"`
	Files []string `yaml:"files"`
	// readonly: read allowed, no write.
	ReadOnly []string `yaml:"readonly"`
	NoWrite  []string `yaml:"no-write"`
	Protect  []string `yaml:"protect"`
}

// resolveBlocks walks from o.pwd up to the filesystem root collecting every
// .vxv.yaml, and stores the merged, absolute, deduplicated lists on o.hide and
// o.readonly. A path listed as both hide and readonly is treated as hide (the
// stronger control). A malformed or unreadable config is reported as a warning
// and skipped rather than aborting the launch — the policy file is advisory
// tooling, not a boot dependency.
func (o *options) resolveBlocks() {
	hideSeen := map[string]bool{}
	roSeen := map[string]bool{}

	for _, dir := range ancestorDirs(o.pwd) {
		path := filepath.Join(dir, blockConfigName)
		hide, ro, err := readBlockConfig(path, dir)
		if err != nil {
			if !os.IsNotExist(err) {
				fmt.Fprintf(os.Stderr, "vxv: warning: ignoring %s: %v\n", path, err)
			}
			continue
		}
		for _, p := range hide {
			if !hideSeen[p] {
				hideSeen[p] = true
				o.hide = append(o.hide, p)
			}
		}
		for _, p := range ro {
			if !roSeen[p] {
				roSeen[p] = true
				o.readonly = append(o.readonly, p)
			}
		}
	}

	// hide dominates readonly for any path in both.
	if len(o.readonly) > 0 && len(hideSeen) > 0 {
		kept := o.readonly[:0]
		for _, p := range o.readonly {
			if !hideSeen[p] {
				kept = append(kept, p)
			}
		}
		o.readonly = kept
	}
}

// ancestorDirs returns dir and each of its parents up to the filesystem root,
// nearest first.
func ancestorDirs(dir string) []string {
	var dirs []string
	for {
		dirs = append(dirs, dir)
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return dirs
}

// readBlockConfig parses one .vxv.yaml and returns its hide and readonly entries
// as cleaned absolute paths. Relative entries resolve against baseDir (the
// config's own directory). The bare-sequence form (a top-level YAML list) is
// accepted as shorthand for `hide`. A path containing a comma is dropped: the
// lists are handed to the guest joined by commas.
func readBlockConfig(path, baseDir string) (hide, readonly []string, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}

	var cfg blockConfig
	keyedErr := yaml.Unmarshal(data, &cfg)

	var rawHide, rawRO []string
	if keyedErr == nil {
		rawHide = append(rawHide, cfg.Hide...)
		rawHide = append(rawHide, cfg.Deny...)
		rawHide = append(rawHide, cfg.Block...)
		rawHide = append(rawHide, cfg.Files...)
		rawRO = append(rawRO, cfg.ReadOnly...)
		rawRO = append(rawRO, cfg.NoWrite...)
		rawRO = append(rawRO, cfg.Protect...)
	}

	// Fall back to a bare top-level sequence (shorthand for hide) only when the
	// keyed shape produced nothing.
	if len(rawHide) == 0 && len(rawRO) == 0 {
		var bare []string
		if err := yaml.Unmarshal(data, &bare); err == nil && len(bare) > 0 {
			rawHide = bare
		} else if keyedErr != nil {
			// Nothing matched and the file is genuinely malformed: surface it.
			return nil, nil, keyedErr
		}
	}

	return cleanEntries(rawHide, baseDir, path), cleanEntries(rawRO, baseDir, path), nil
}

// cleanEntries turns raw config entries into cleaned absolute paths: trims blanks,
// resolves relatives against baseDir, and drops entries containing a comma (the
// list delimiter used to hand paths to the guest).
func cleanEntries(entries []string, baseDir, path string) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if !filepath.IsAbs(e) {
			e = filepath.Join(baseDir, e)
		}
		e = filepath.Clean(e)
		if strings.Contains(e, ",") {
			fmt.Fprintf(os.Stderr, "vxv: warning: %s: dropping entry with comma: %q\n", path, e)
			continue
		}
		out = append(out, e)
	}
	return out
}
