// Copyright 2015 CoreOS, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package files

import (
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"syscall"

	configUtil "github.com/coreos/ignition/config/util"
	"github.com/coreos/ignition/internal/config/types"
	"github.com/coreos/ignition/internal/exec/stages"
	"github.com/coreos/ignition/internal/exec/util"
	"github.com/coreos/ignition/internal/image"
	"github.com/coreos/ignition/internal/log"
	"github.com/coreos/ignition/internal/resource"
)

const (
	name = "files"
)

var (
	ErrFilesystemUndefined = errors.New("the referenced filesystem was not defined")
)

func init() {
	stages.Register(creator{})
}

type creator struct{}

func (creator) Create(logger *log.Logger, root string, f resource.Fetcher) stages.Stage {
	return &stage{
		Util: util.Util{
			DestDir: root,
			Logger:  logger,
			Fetcher: f,
		},
	}
}

func (creator) Name() string {
	return name
}

type stage struct {
	util.Util
}

func (stage) Name() string {
	return name
}

func (s stage) Run(config types.Config) bool {
	if err := s.createPasswd(config); err != nil {
		s.Logger.Crit("failed to create users/groups: %v", err)
		return false
	}

	if err := s.createFilesystemsEntries(config); err != nil {
		s.Logger.Crit("failed to create files: %v", err)
		return false
	}

	if err := s.createUnits(config); err != nil {
		s.Logger.Crit("failed to create units: %v", err)
		return false
	}

	return true
}

// createFilesystemsEntries creates the files described in config.Storage.{Files,Directories}.
func (s stage) createFilesystemsEntries(config types.Config) error {
	if len(config.Storage.Filesystems) == 0 {
		return nil
	}
	s.Logger.PushPrefix("createFilesystemsFiles")
	defer s.Logger.PopPrefix()

	entryMap, err := s.mapEntriesToFilesystems(config)
	if err != nil {
		return err
	}

	for _, fs := range config.Storage.Filesystems {
                f := entryMap[fs.Name]
		if f == nil {
			f = []filesystemEntry{}
		}
		if err := s.createEntries(fs, f); err != nil {
			return fmt.Errorf("failed to create files: %v", err)
		}
	}

	return nil
}

// filesystemEntry represent a thing that knows how to create itself.
type filesystemEntry interface {
	create(l *log.Logger, u util.Util) error
}

type fileEntry types.File

func (tmp fileEntry) create(l *log.Logger, u util.Util) error {
	f := types.File(tmp)

	fetchOp := u.PrepareFetch(l, f)
	if fetchOp == nil {
		return fmt.Errorf("failed to resolve file %q", f.Path)
	}

	msg := "writing file %q"
	if f.Append {
		msg = "appending to file %q"
	}

	if err := l.LogOp(
		func() error {
			err := u.DeletePathOnOverwrite(f.Node)
			if err != nil {
				return err
			}

			return u.PerformFetch(fetchOp)
		}, msg, string(f.Path),
	); err != nil {
		return fmt.Errorf("failed to create file %q: %v", fetchOp.Path, err)
	}

	return nil
}

type dirEntry types.Directory

func (tmp dirEntry) create(l *log.Logger, u util.Util) error {
	d := types.Directory(tmp)

	err := l.LogOp(func() error {
		path := filepath.Clean(u.JoinPath(string(d.Path)))

		err := u.DeletePathOnOverwrite(d.Node)
		if err != nil {
			return err
		}

		uid, gid, err := u.ResolveNodeUidAndGid(d.Node, 0, 0)
		if err != nil {
			return err
		}

		// Build a list of paths to create. Since os.MkdirAll only sets the mode for new directories and not the
		// ownership, we need to determine which directories will be created so we don't chown something that already
		// exists.
		newPaths := []string{path}
		for p := filepath.Dir(path); p != "/"; p = filepath.Dir(p) {
			_, err := os.Stat(p)
			if err == nil {
				break
			}
			if !os.IsNotExist(err) {
				return err
			}
			newPaths = append(newPaths, p)
		}

		if d.Mode == nil {
			d.Mode = configUtil.IntToPtr(0)
		}

		if err := os.MkdirAll(path, os.FileMode(*d.Mode)); err != nil {
			return err
		}

		for _, newPath := range newPaths {
			if err := os.Chmod(newPath, os.FileMode(*d.Mode)); err != nil {
				return err
			}
			if err := os.Chown(newPath, uid, gid); err != nil {
				return err
			}
		}

		return nil
	}, "creating directory %q", string(d.Path))
	if err != nil {
		return fmt.Errorf("failed to create directory %q: %v", d.Path, err)
	}

	return nil
}

type linkEntry types.Link

func (tmp linkEntry) create(l *log.Logger, u util.Util) error {
	s := types.Link(tmp)

	if err := l.LogOp(
		func() error {
			err := u.DeletePathOnOverwrite(s.Node)
			if err != nil {
				return err
			}

			return u.WriteLink(s)
		}, "writing link %q -> %q", s.Path, s.Target,
	); err != nil {
		return fmt.Errorf("failed to create link %q: %v", s.Path, err)
	}

	return nil
}

// ByDirectorySegments is used to sort directories so /foo gets created before /foo/bar if they are both specified.
type ByDirectorySegments []types.Directory

func (lst ByDirectorySegments) Len() int { return len(lst) }

func (lst ByDirectorySegments) Swap(i, j int) {
	lst[i], lst[j] = lst[j], lst[i]
}

func (lst ByDirectorySegments) Less(i, j int) bool {
	return depth(lst[i].Node) < depth(lst[j].Node)
}

func depth(n types.Node) uint {
	var count uint = 0
	for p := filepath.Clean(string(n.Path)); p != "/"; count++ {
		p = filepath.Dir(p)
	}
	return count
}

// mapEntriesToFilesystems builds a map of filesystem names to files. If multiple
// definitions of the same filesystem are present, only the final definition is
// used. The directories are sorted to ensure /foo gets created before /foo/bar.
func (s stage) mapEntriesToFilesystems(config types.Config) (map[string][]filesystemEntry, error) {
	filesystems := map[string]types.Filesystem{}
        mountPoints := map[string]*string{}
	fsparts := [][]string{
		[]string{ "devpts", "/dev/pts", "devpts", "gid=5,mode=620", "0", "0" },
		[]string{ "tmpfs", "/dev/shm", "tmpfs", "defaults", "0", "0" },
		[]string{ "proc", "/proc", "proc", "defaults", "0", "0" },
		[]string{ "sysfs", "/sys", "sysfs", "defaults", "0", "0" },
	}
	var rootFs *types.Filesystem
	for _, fs := range config.Storage.Filesystems {
		filesystems[fs.Name] = fs
		if fs.Mount != nil && fs.Mount.Point != nil {
			mountPoints[fs.Name] = fs.Mount.Point
			fsparts = append(fsparts, []string{
				fmt.Sprintf("LABEL=%s", fs.Mount.Label),
				*fs.Mount.Point,
				fs.Mount.Format,
				"defaults",
				"0",
				"0"})
		}
		if fs.Mount.Label != nil && *fs.Mount.Label == "root" {
			rootFs = &fs
		}
	}

	entryMap := map[string][]filesystemEntry{}

	// Sort directories to ensure /a gets created before /a/b.
	sortedDirs := config.Storage.Directories
	sort.Sort(ByDirectorySegments(sortedDirs))

	// Add directories first to ensure they are created before files.
	for _, d := range sortedDirs {
		if fs, ok := filesystems[d.Filesystem]; ok {
			entryMap[fs.Name] = append(entryMap[fs.Name], dirEntry(d))
		} else {
			s.Logger.Crit("the filesystem (%q), was not defined", d.Filesystem)
			return nil, ErrFilesystemUndefined
		}
	}
	// Make sure all mount point directories exist
	if rootFs != nil && len(mountPoints) > 0 {
		for _, dir := range mountPoints {
			d := dirEntry{ Node: types.Node{Path: *dir, Filesystem: rootFs.Name} }
			entryMap[rootFs.Name] = append(entryMap[rootFs.Name], d)
		}
	}

	for _, f := range config.Storage.Files {
		if fs, ok := filesystems[f.Filesystem]; ok {
			entryMap[fs.Name] = append(entryMap[fs.Name], fileEntry(f))
		} else {
			s.Logger.Crit("the filesystem (%q), was not defined", f.Filesystem)
			return nil, ErrFilesystemUndefined
		}
	}

	for _, sy := range config.Storage.Links {
		if fs, ok := filesystems[sy.Filesystem]; ok {
			entryMap[fs.Name] = append(entryMap[fs.Name], linkEntry(sy))
		} else {
			s.Logger.Crit("the filesystem (%q), was not defined", sy.Filesystem)
			return nil, ErrFilesystemUndefined
		}
	}

	// Root file system and we have mount points.
	// Build an /etc/fstab
	if rootFs != nil && len(mountPoints) > 0 {
			f := fileEntry{ Node: types.Node{Path: "etc/fstab", Filesystem: rootFs.Name} }
			entryMap[rootFs.Name] = append(entryMap[rootFs.Name], f)
	}

	return entryMap, nil
}

// createEntries creates any files or directories listed for the filesystem in Storage.{Files,Directories}.
// Additionally will apply images and bootloader files as needed.
func (s stage) createEntries(fs types.Filesystem, files []filesystemEntry) error {
	s.Logger.PushPrefix("createFiles")
	defer s.Logger.PopPrefix()

	var mnt string
	if fs.Path == nil {
		var err error
		mnt, err = ioutil.TempDir("", "ignition-files")
		if err != nil {
			return fmt.Errorf("failed to create temp directory: %v", err)
		}
		defer os.Remove(mnt)

		dev := string(fs.Mount.Device)
		format := string(fs.Mount.Format)

		if err := s.Logger.LogOp(
			func() error {
				if format != "ntfs" {
					return syscall.Mount(dev, mnt, format, 0, "")
				}
				cmd := fmt.Sprintf("mount -t ntfs %s %s", dev, mnt)
				_, err := exec.Command("bash", "-c", cmd).Output()
				return err
			},
			"mounting %q at %q", dev, mnt,
		); err != nil {
			return fmt.Errorf("failed to mount device %q at %q: %v", dev, mnt, err)
		}
		defer s.Logger.LogOp(
			func() error {
				if format != "ntfs" {
					return syscall.Unmount(mnt, 0) 
				}
				cmd := fmt.Sprintf("umount %s", mnt)
				_, err := exec.Command("bash", "-c", cmd).Output()
				return err
			},
			"unmounting %q at %q", dev, mnt,
		)
	} else {
		mnt = *fs.Path
	}

	u := util.Util{
		DestDir: mnt,
		Fetcher: s.Util.Fetcher,
		Logger:  s.Logger,
	}

	for _, img := range fs.Images {
		if err := image.ApplyImage(s.Logger, img, mnt); err != nil {
			return err
		}
	}

	for _, e := range files {
		if err := e.create(s.Logger, u); err != nil {
			return err
		}
	}

	// If the file system is marked to be booted from, install the boot loader pieces.
	// This will add syslinux files to a /boot or /Boot directory, place a config file,
        // and install the gptmgr bootloader.
	if fs.Mount != nil && fs.Mount.BootFilesystem {

	}

	return nil
}

// createUnits creates the units listed under systemd.units and networkd.units.
func (s stage) createUnits(config types.Config) error {
	for _, unit := range config.Systemd.Units {
		if err := s.writeSystemdUnit(unit); err != nil {
			return err
		}
		if unit.Enable {
			s.Logger.Warning("the enable field has been deprecated in favor of enabled")
			if err := s.Logger.LogOp(
				func() error { return s.EnableUnit(unit) },
				"enabling unit %q", unit.Name,
			); err != nil {
				return err
			}
		}
		if unit.Enabled != nil {
			if *unit.Enabled {
				if err := s.Logger.LogOp(
					func() error { return s.EnableUnit(unit) },
					"enabling unit %q", unit.Name,
				); err != nil {
					return err
				}
			} else {
				if err := s.Logger.LogOp(
					func() error { return s.DisableUnit(unit) },
					"disabling unit %q", unit.Name,
				); err != nil {
					return err
				}
			}
		}
		if unit.Mask {
			if err := s.Logger.LogOp(
				func() error { return s.MaskUnit(unit) },
				"masking unit %q", unit.Name,
			); err != nil {
				return err
			}
		}
	}
	for _, unit := range config.Networkd.Units {
		if err := s.writeNetworkdUnit(unit); err != nil {
			return err
		}
	}
	return nil
}

// writeSystemdUnit creates the specified unit and any dropins for that unit.
// If the contents of the unit or are empty, the unit is not created. The same
// applies to the unit's dropins.
func (s stage) writeSystemdUnit(unit types.Unit) error {
	return s.Logger.LogOp(func() error {
		for _, dropin := range unit.Dropins {
			if dropin.Contents == "" {
				continue
			}

			f, err := util.FileFromSystemdUnitDropin(unit, dropin)
			if err != nil {
				s.Logger.Crit("error converting systemd dropin: %v", err)
				return err
			}
			if err := s.Logger.LogOp(
				func() error { return s.PerformFetch(f) },
				"writing systemd drop-in %q at %q", dropin.Name, f.Path,
			); err != nil {
				return err
			}
		}

		if unit.Contents == "" {
			return nil
		}

		f, err := util.FileFromSystemdUnit(unit)
		if err != nil {
			s.Logger.Crit("error converting unit: %v", err)
			return err
		}
		if err := s.Logger.LogOp(
			func() error { return s.PerformFetch(f) },
			"writing unit %q at %q", unit.Name, f.Path,
		); err != nil {
			return err
		}

		return nil
	}, "processing unit %q", unit.Name)
}

// writeNetworkdUnit creates the specified unit and any dropins for that unit.
// If the contents of the unit or are empty, the unit is not created. The same
// applies to the unit's dropins.
func (s stage) writeNetworkdUnit(unit types.Networkdunit) error {
	return s.Logger.LogOp(func() error {
		for _, dropin := range unit.Dropins {
			if dropin.Contents == "" {
				continue
			}

			f, err := util.FileFromNetworkdUnitDropin(unit, dropin)
			if err != nil {
				s.Logger.Crit("error converting networkd dropin: %v", err)
				return err
			}
			if err := s.Logger.LogOp(
				func() error { return s.PerformFetch(f) },
				"writing networkd drop-in %q at %q", dropin.Name, f.Path,
			); err != nil {
				return err
			}
		}
		if unit.Contents == "" {
			return nil
		}

		f, err := util.FileFromNetworkdUnit(unit)
		if err != nil {
			s.Logger.Crit("error converting unit: %v", err)
			return err
		}
		if err := s.Logger.LogOp(
			func() error { return s.PerformFetch(f) },
			"writing unit %q at %q", unit.Name, f.Path,
		); err != nil {
			return err
		}

		return nil
	}, "processing unit %q", unit.Name)
}

// createPasswd creates the users and groups as described in config.Passwd.
func (s stage) createPasswd(config types.Config) error {
	if err := s.createGroups(config); err != nil {
		return fmt.Errorf("failed to create groups: %v", err)
	}

	if err := s.createUsers(config); err != nil {
		return fmt.Errorf("failed to create users: %v", err)
	}

	return nil
}

// createUsers creates the users as described in config.Passwd.Users.
func (s stage) createUsers(config types.Config) error {
	if len(config.Passwd.Users) == 0 {
		return nil
	}
	s.Logger.PushPrefix("createUsers")
	defer s.Logger.PopPrefix()

	for _, u := range config.Passwd.Users {
		if err := s.EnsureUser(u); err != nil {
			return fmt.Errorf("failed to create user %q: %v",
				u.Name, err)
		}

		if err := s.SetPasswordHash(u); err != nil {
			return fmt.Errorf("failed to set password for %q: %v",
				u.Name, err)
		}

		if err := s.AuthorizeSSHKeys(u); err != nil {
			return fmt.Errorf("failed to add keys to user %q: %v",
				u.Name, err)
		}
	}

	return nil
}

// createGroups creates the users as described in config.Passwd.Groups.
func (s stage) createGroups(config types.Config) error {
	if len(config.Passwd.Groups) == 0 {
		return nil
	}
	s.Logger.PushPrefix("createGroups")
	defer s.Logger.PopPrefix()

	for _, g := range config.Passwd.Groups {
		if err := s.CreateGroup(g); err != nil {
			return fmt.Errorf("failed to create group %q: %v",
				g.Name, err)
		}
	}

	return nil
}
