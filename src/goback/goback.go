package main

import (
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path"
	"strings"
	"sudo"
	"syscall"
	"time"
)

func main() {
	log.Printf("Godump!")

	var err error
	conf, err := loadConfig()
	if err != nil {
		log.Printf("Unable to load config file: %s", err)
		return
	}

	host, err := os.Hostname()
	if err != nil {
		log.Fatalf("Unable to get current hostname: %s", err)
	}
	log.Printf("Hostname: %q", host)

	info, ok := conf[host]
	if !ok {
		log.Fatalf("Host %q not found in config file", host)
	}

	namer := newNamer()

	// log.Printf("info: %#v", info)
	// for _, fs := range info.Filesystems {
	// 	log.Printf("fs: %#v", fs)
	// 	log.Printf("volname: %q", namer.Snapvol(fs))
	// 	log.Printf("dev: %q", namer.Snapdev(fs))
	// }

	lvm, err := GetLVM()
	if err != nil {
		log.Fatalf("Error getting lvm info: %s", err)
	}

	// The various parts of the backup.
	var backup Backup
	backup.conf = conf
	backup.namer = namer
	backup.host = info
	backup.lvm = lvm

	err = backup.MakeSnap()
	if err != nil {
		// TODO: Undo the backup.
		log.Fatalf("Error making snapshot: %s", err)
	}

	err = backup.GoSure()
	if err != nil {
		log.Fatalf("Error running gosure: %s", err)
	}
}

// This probably should be in the config file.
var gosurePath = "/home/davidb/bin/gosure"

type Backup struct {
	conf  Config
	namer *Namer
	host  *Host
	lvm   *LVInfo
}

func (b *Backup) MakeSnap() (err error) {
	// Verify that today's snapshot doesn't exist.
	for _, fs := range b.host.Filesystems {
		// log.Printf("Checking %s", fs)
		snap := b.namer.SnapVgName(fs)

		if b.lvm.HasSnap(snap) {
			err = errors.New(fmt.Sprintf("Volume %s already present", snap))
			return
		}
	}

	// Now construct the snapshots.
	for _, fs := range b.host.Filesystems {
		base := fs.VgName()
		snap := b.namer.SnapVgName(fs)
		err = snapshot(base, snap)
		if err != nil {
			return
		}
	}

	return
}

// Invoke gosure on the snapshots.
func (b *Backup) GoSure() (err error) {
	for _, fs := range b.host.Filesystems {
		err = b.goSureOne(fs)
		if err != nil {
			return
		}
	}

	return
}

func (b *Backup) goSureOne(fs *FsInfo) (err error) {
	snap := b.namer.SnapVgName(fs)
	smount := b.snapName(fs)

	// Activate the VG.
	err = b.activate(snap)
	if err != nil {
		return
	}
	defer b.deactivate(snap)

	// for sanity sake, run a fsck.
	err = b.fsck(snap)
	if err != nil {
		return
	}

	// Mount it.
	err = b.mount(snap, smount, false)
	if err != nil {
		return
	}
	defer b.umount(snap)

	err = b.runGosure(fs)
	if err != nil {
		return
	}

	return
}

// Get the name of the mountpoint for this particular filesystem.
func (b *Backup) snapName(fs *FsInfo) string {
	return path.Join(b.host.Snapdir, fs.Lvname)
}

func (b *Backup) activate(vol VgName) (err error) {
	sudo.Setup()

	cmd := exec.Command("lvchange", "-ay", "-K", vol.DevName())
	cmd = sudo.Sudoify(cmd)
	showCommand(cmd)
	err = cmd.Run()
	return
}

func (b *Backup) deactivate(vol VgName) (err error) {
	sudo.Setup()

	cmd := exec.Command("lvchange", "-an", vol.DevName())
	cmd = sudo.Sudoify(cmd)
	showCommand(cmd)
	err = cmd.Run()
	return
}

func (b *Backup) mount(vol VgName, dest string, writable bool) (err error) {
	sudo.Setup()

	flags := make([]string, 0, 4)

	if writable {
		flags = append(flags, "-r")
	}
	flags = append(flags, vol.DevName())
	flags = append(flags, dest)

	cmd := exec.Command("mount", flags...)
	cmd = sudo.Sudoify(cmd)
	showCommand(cmd)
	err = cmd.Run()
	return
}

func (b *Backup) umount(vol VgName) (err error) {
	sudo.Setup()

	cmd := exec.Command("umount", vol.DevName())
	cmd = sudo.Sudoify(cmd)
	showCommand(cmd)
	err = cmd.Run()
	return
}

func (b *Backup) fsck(vol VgName) (err error) {
	sudo.Setup()

	cmd := exec.Command("fsck", "-p", "-f", vol.DevName())
	cmd = sudo.Sudoify(cmd)
	showCommand(cmd)
	err = cmd.Run()
	if err != nil {
		// Some unsuccessful results are fine.
		stat := cmd.ProcessState.Sys().(syscall.WaitStatus).ExitStatus()
		log.Printf("Status: %d", stat)
		if stat == 1 {
			err = nil
		}
	}
	// Successful error status is fine.
	return
}

func (b *Backup) runGosure(fs *FsInfo) (err error) {
	sudo.Setup()

	// TODO: Detect no 2sure.dat.gz file, and run a fresh gosure
	// instead of this scan.

	place := path.Join(fs.Mount, "2sure")

	cmd := exec.Command(gosurePath, "-file", place, "update")
	cmd = sudo.Sudoify(cmd)
	cmd.Dir = b.snapName(fs)
	showCommand(cmd)
	err = cmd.Run()

	// TODO: Run signoff and capture the output.

	// TODO: Copy the 2sure.dat and .bak into the snapshot.

	return
}

// Backup date/time.
type Namer struct {
	date string
}

func newNamer() *Namer {
	var result Namer

	result.date = time.Now().Local().Format("2006.01.02")

	return &result
}

func (n *Namer) Snapvol(fs *FsInfo) string {
	return fmt.Sprintf("%s.%s", fs.Lvname, n.date)
}

func (n *Namer) Snapdev(fs *FsInfo) string {
	return fmt.Sprintf("/dev/mapper/%s-%s", fs.Volgroup, n.Snapvol(fs))
}

func (n *Namer) SnapVgName(fs *FsInfo) VgName {
	return VgName{VG: fs.Volgroup, LV: n.Snapvol(fs)}
}

func snapshot(base, snap VgName) (err error) {
	sudo.Setup()

	cmd := exec.Command("lvcreate", "-s",
		base.TextName(), "-n", snap.LV)
	cmd = sudo.Sudoify(cmd)

	showCommand(cmd)

	err = cmd.Run()

	return
}

func showCommand(cmd *exec.Cmd) {
	log.Printf("%s", strings.Join(cmd.Args, " "))

	if cmd.Dir != "" {
		log.Printf("  in dir: %q", cmd.Dir)
	}
}