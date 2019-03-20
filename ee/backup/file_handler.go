// +build !oss

/*
 * Copyright 2018 Dgraph Labs, Inc. and Contributors
 *
 * Licensed under the Dgraph Community License (the "License"); you
 * may not use this file except in compliance with the License. You
 * may obtain a copy of the License at
 *
 *     https://github.com/dgraph-io/dgraph/blob/master/licenses/DCL.txt
 */

package backup

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/dgraph-io/dgraph/x"

	"github.com/golang/glog"
)

// fileHandler is used for 'file:' URI scheme.
type fileHandler struct {
	fp *os.File
}

// readManifest reads a manifest file at path using the handler.
// Returns nil on success, otherwise an error.
func (h *fileHandler) readManifest(path string, m *Manifest) error {
	b, err := ioutil.ReadFile(path)
	if err != nil {
		return err
	}
	m.Lock()
	defer m.Unlock()
	return json.Unmarshal(b, m)
}

// Create prepares the a path to save backup files.
// Returns error on failure, nil on success.
func (h *fileHandler) Create(uri *url.URL, req *Request) error {
	var dir, path, fileName string

	// check that the path exists and we can access it.
	if !h.exists(uri.Path) {
		return x.Errorf("The path %q does not exist or it is inaccessible.", uri.Path)
	}

	// Find the max version from the latest backup. This is done only when starting a new
	// backup, not when creating a manifest.
	if req.Manifest == nil {
		// Walk the path and find the most recent backup.
		// If we can't find a manifest file, this is a full backup.
		var lastManifest string
		suffix := filepath.Join(string(filepath.Separator), backupManifest)
		_ = x.WalkPathFunc(uri.Path, func(path string, isdir bool) bool {
			if !isdir && strings.HasSuffix(path, suffix) && path > lastManifest {
				lastManifest = path
			}
			return false
		})
		// Found a manifest now we extract the version to use in Backup().
		if lastManifest != "" {
			var m Manifest
			if err := h.readManifest(lastManifest, &m); err != nil {
				return err
			}
			// No new changes since last check
			if m.Version == req.Backup.SnapshotTs {
				return ErrBackupNoChanges
			}
			// Return the version of last backup
			req.Version = m.Version
		}
		fileName = fmt.Sprintf(backupNameFmt, req.Backup.ReadTs, req.Backup.GroupId)
	} else {
		fileName = backupManifest
	}

	dir = filepath.Join(uri.Path, fmt.Sprintf(backupPathFmt, req.Backup.UnixTs))
	if err := os.Mkdir(dir, 0700); err != nil && !os.IsExist(err) {
		return err
	}

	path = filepath.Join(dir, fileName)
	fp, err := os.Create(path)
	if err != nil {
		return err
	}
	glog.V(2).Infof("Using file path: %q", path)
	h.fp = fp

	return nil
}

// Load uses tries to load any backup files found.
// Returns nil and the maximum Ts version on success, error otherwise.
func (h *fileHandler) Load(uri *url.URL, fn loadFn) (uint64, error) {
	if !h.exists(uri.Path) {
		return 0, x.Errorf("The path %q does not exist or it is inaccessible.", uri.Path)
	}

	// Get a lisst of all the manifest files at the location.
	suffix := filepath.Join(string(filepath.Separator), backupManifest)
	manifests := x.WalkPathFunc(uri.Path, func(path string, isdir bool) bool {
		return !isdir && strings.HasSuffix(path, suffix)
	})
	if len(manifests) == 0 {
		return 0, x.Errorf("No manifests found at path: %s", uri.Path)
	}
	sort.Strings(manifests)
	if glog.V(3) {
		fmt.Printf("Found backup manifest(s): %v\n", manifests)
	}

	// version is returned with the max manifest version found.
	var version uint64

	// Process each manifest, first check that they are valid and then confirm the
	// backup files for each group exist. Each group in manifest must have a backup file,
	// otherwise this is a failure and the user must remedy.
	for _, manifest := range manifests {
		var m Manifest
		if err := h.readManifest(manifest, &m); err != nil {
			return 0, x.Errorf("Error while reading manifests: %v", err)
		}
		if m.ReadTs == 0 || m.Version == 0 || len(m.Groups) == 0 {
			if glog.V(2) {
				fmt.Printf("Restore: skip backup: %s: %#v\n", manifest, &m)
			}
			continue
		}

		// Load the backup for each group in manifest.
		path := filepath.Dir(manifest)
		for _, groupId := range m.Groups {
			file := filepath.Join(path, fmt.Sprintf(backupNameFmt, m.ReadTs, groupId))
			fp, err := os.Open(file)
			if err != nil {
				return 0, x.Errorf("Error opening %q: %s", file, err)
			}
			defer fp.Close()
			if err = fn(fp, int(groupId)); err != nil {
				return 0, err
			}
		}
		version = m.Version
	}
	return version, nil
}

func (h *fileHandler) Close() error {
	if h.fp == nil {
		return nil
	}
	if err := h.fp.Sync(); err != nil {
		glog.Errorf("While closing file: %s. Error: %v", h.fp.Name(), err)
		x.Ignore(h.fp.Close())
		return err
	}
	return h.fp.Close()
}

func (h *fileHandler) Write(b []byte) (int, error) {
	return h.fp.Write(b)
}

// Exists checks if a path (file or dir) is found at target.
// Returns true if found, false otherwise.
func (h *fileHandler) exists(path string) bool {
	_, err := os.Stat(path)
	if err == nil {
		return true
	}
	return !os.IsNotExist(err) && !os.IsPermission(err)
}
