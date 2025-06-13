// dirsync.go  –  Win-7/Win-10 directory sync (FTP or SMB)
//
// Build inside WSL / Linux:
//   export CGO_ENABLED=0 GOOS=windows GOARCH=amd64
//   go mod tidy
//   go build -ldflags "-s -w" -o dirsync.exe
//
// Run on Windows:
//   dirsync.exe -conf dataxfer.conf
//
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/jlaffaye/ftp"
)

type SMBConf struct {
	Host, User, Pass, Share, RemotePath string
}
type FTPConf struct {
	Host, User, Pass, RemotePath string
}
type Conf struct {
	LocalDir string  `json:"local_dir"`
	Type     string  `json:"type"` // "smb" | "ftp"
	SMB      SMBConf `json:"smb"`
	FTP      FTPConf `json:"ftp"`
}

func loadConf(p string) (*Conf, error) {
	f, err := os.Open(p)
	if err != nil { return nil, err }
	defer f.Close()
	var c Conf
	return &c, json.NewDecoder(f).Decode(&c)
}

func newer(local, remote time.Time) bool { return remote.IsZero() || local.After(remote) }

// ────────── FTP target ──────────────────────────────────────
type ftpTarget struct {
	c      *ftp.ServerConn
	prefix string
}

func connectFTP(cfg FTPConf) (*ftpTarget, error) {
	conn, err := ftp.Dial(cfg.Host, ftp.DialWithTimeout(10*time.Second))
	if err != nil { return nil, err }
	if err = conn.Login(cfg.User, cfg.Pass); err != nil { return nil, err }
	return &ftpTarget{c: conn, prefix: cfg.RemotePath}, nil
}

func (t *ftpTarget) mtime(rel string) (time.Time, error) {
	remoteDir := filepath.ToSlash(filepath.Join(t.prefix, filepath.Dir(rel)))
	entries, err := t.c.List(remoteDir)
	if err != nil { return time.Time{}, err }
	base := filepath.Base(rel)
	for _, e := range entries {
		if e.Name == base {
			return e.Time, nil
		}
	}
	return time.Time{}, os.ErrNotExist
}

func (t *ftpTarget) upload(local, rel string) error {
	remote := filepath.ToSlash(filepath.Join(t.prefix, rel))
	dir := filepath.Dir(remote)
	// create directory chain
	if dir != "" && dir != "." {
		dirs := strings.Split(dir, "/")
		p := ""
		for _, d := range dirs {
			p = filepath.Join(p, d)
			t.c.MakeDir(p)
		}
	}
	src, err := os.Open(local)
	if err != nil { return err }
	defer src.Close()
	return t.c.Stor(remote, src)
}
func (t *ftpTarget) close() { t.c.Quit() }

// ────────── SMB target (net use) ────────────────────────────
type smbTarget struct {
	drive, unc, prefix string
}

func connectSMB(cfg SMBConf) (*smbTarget, error) {
	host := strings.Split(cfg.Host, ":")[0]
	unc  := fmt.Sprintf(`\\%s\%s`, host, cfg.Share)
	drive := "Z:"
	if out, err := exec.Command("net", "use", drive, unc, cfg.Pass, "/user:"+cfg.User, "/persistent:no").CombinedOutput(); err != nil {
		return nil, fmt.Errorf("net use: %v – %s", err, out)
	}
	return &smbTarget{drive: drive, unc: unc, prefix: cfg.RemotePath}, nil
}

func (t *smbTarget) toRemote(rel string) string {
	if t.prefix != "" { rel = filepath.Join(t.prefix, rel) }
	return filepath.Join(t.drive, rel)
}
func (t *smbTarget) mtime(rel string) (time.Time, error) {
	fi, err := os.Stat(t.toRemote(rel))
	if err != nil { return time.Time{}, err }
	return fi.ModTime(), nil
}
func (t *smbTarget) upload(local, rel string) error {
	dst := t.toRemote(rel)
	os.MkdirAll(filepath.Dir(dst), fs.FileMode(0755))
	src, err := os.Open(local)
	if err != nil { return err }
	defer src.Close()

	tmp := dst + ".tmp"
	out, err := os.Create(tmp)
	if err != nil { return err }
	if _, err = io.Copy(out, src); err != nil {
		out.Close(); return err
	}
	out.Close()
	return os.Rename(tmp, dst)
}
func (t *smbTarget) close() { exec.Command("net", "use", t.drive, "/delete", "/y").Run() }

// ────────── main sync logic ────────────────────────────────
func main() {
	cfgPath := flag.String("conf", "dataxfer.conf", "config JSON")
	flag.Parse()

	conf, err := loadConf(*cfgPath)
	if err != nil { log.Fatal(err) }

	var (
		getMTime func(string) (time.Time, error)
		putFile  func(string, string) error
		closeFn  func()
	)

	switch strings.ToLower(conf.Type) {
	case "ftp":
		ft, err := connectFTP(conf.FTP); if err != nil { log.Fatal(err) }
		getMTime, putFile, closeFn = ft.mtime, ft.upload, ft.close
	case "smb":
		st, err := connectSMB(conf.SMB); if err != nil { log.Fatal(err) }
		getMTime, putFile, closeFn = st.mtime, st.upload, st.close
	default:
		log.Fatalf("unknown type: %s (use 'ftp' or 'smb')", conf.Type)
	}
	defer closeFn()

	root := conf.LocalDir
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() { return walkErr }
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)

		localInfo, _ := os.Stat(path)
		remoteTime, _ := getMTime(rel)

		if newer(localInfo.ModTime(), remoteTime) {
			fmt.Printf("↑ %s\n", rel)
			if err := putFile(path, rel); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Fatal(err)
	}
	fmt.Println("✓ Sync complete")
}
