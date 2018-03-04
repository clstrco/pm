package pkg

import (
	"archive/tar"
	"bufio"
	"crypto/sha256"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/pkg/errors"
	"mcquay.me/fs"
	"mcquay.me/pm"
	"mcquay.me/pm/db"
	"mcquay.me/pm/keyring"
)

const cache = "var/cache/pm"

// Install fetches and installs pkgs from appropriate remotes.
func Install(root string, pkgs []string) error {
	av, err := db.LoadAvailable(root)
	if err != nil {
		return errors.Wrap(err, "loading available db")
	}

	ms, err := av.Installable(pkgs)
	if err != nil {
		return errors.Wrap(err, "checking ability to install")
	}

	cacheDir := filepath.Join(root, cache)
	if !fs.Exists(cacheDir) {
		if err := os.MkdirAll(cacheDir, 0755); err != nil {
			return errors.Wrap(err, "creating non-existent cache dir")
		}
	}
	if !fs.IsDir(cacheDir) {
		return errors.Errorf("%q is not a directory!", cacheDir)
	}

	if err := download(cacheDir, ms); err != nil {
		return errors.Wrap(err, "downloading")
	}

	for _, m := range ms {
		log.Printf("%+v", m)
		if err := verifyManifestIntegrity(root, m); err != nil {
			return errors.Wrap(err, "verifying pkg integrity")
		}
		if err := verifyPkgContents(root, m); err != nil {
			return errors.Wrap(err, "verifying pkg contents")
		}
	}
	return errors.New("NYI")
}

func download(cache string, ms pm.Metas) error {
	// TODO (sm): concurrently fetch
	for _, m := range ms {
		resp, err := http.Get(m.URL())
		if err != nil {
			return errors.Wrap(err, "http get")
		}
		fn := filepath.Join(cache, m.Pkg())
		f, err := os.Create(fn)
		if err != nil {
			return errors.Wrap(err, "creating")
		}

		if n, err := io.Copy(f, resp.Body); err != nil {
			return errors.Wrapf(err, "copy %q to disk after %d bytes", m.URL(), n)
		}

		if err := resp.Body.Close(); err != nil {
			return errors.Wrap(err, "closing resp body")
		}
	}
	return nil
}

func verifyManifestIntegrity(root string, m pm.Meta) error {
	pn := filepath.Join(root, cache, m.Pkg())
	man, err := getReadCloser(pn, "manifest.sha256")
	if err != nil {
		return errors.Wrap(err, "getting manifest reader")
	}
	sig, err := getReadCloser(pn, "manifest.sha256.asc")
	if err != nil {
		return errors.Wrap(err, "getting manifest reader")
	}

	if err := keyring.Verify(root, man, sig); err != nil {
		return errors.Wrap(err, "verifying manifest")
	}
	if err := man.Close(); err != nil {
		return errors.Wrap(err, "closing manifest reader")
	}
	if err := sig.Close(); err != nil {
		return errors.Wrap(err, "closing manifest signature reader")
	}
	return nil
}

func verifyPkgContents(root string, m pm.Meta) error {
	pn := filepath.Join(root, cache, m.Pkg())
	man, err := getReadCloser(pn, "manifest.sha256")
	if err != nil {
		return errors.Wrap(err, "getting manifest reader")
	}

	cs := map[string]string{}
	s := bufio.NewScanner(man)
	for s.Scan() {
		elems := strings.Split(s.Text(), "\t")
		if len(elems) != 2 {
			return errors.Errorf("manifest format error; got %d elements, want 2", len(elems))
		}
		cs[elems[1]] = elems[0]
	}
	if err := man.Close(); err != nil {
		return errors.Wrap(err, "closing manifest reader")
	}
	if err := s.Err(); err != nil {
		return errors.Wrap(err, "scanning manifest")
	}

	pf, err := os.Open(pn)
	if err != nil {
		return errors.Wrap(err, "opening pkg file")
	}
	tr := tar.NewReader(pf)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return errors.Wrap(err, "tar traversal")
		}

		if hdr.Name == "manifest.sha256" || hdr.Name == "manifest.sha256.asc" {
			continue
		}
		if hdr.FileInfo().IsDir() {
			continue
		}
		sha, ok := cs[hdr.Name]
		if !ok {
			return errors.Errorf("extra file %q found in tarfile!", hdr.Name)
		}
		sr := sha256.New()
		if n, err := io.Copy(sr, tr); err != nil {
			return errors.Wrapf(err, "calculating checksum after %v bytes", n)
		}

		if sha != fmt.Sprintf("%x", sr.Sum(nil)) {
			return errors.Errorf("%q checksum was incorrect", hdr.Name)
		}
	}
	return nil
}

type tarSlurper struct {
	f  *os.File
	tr *tar.Reader
}

func (ts *tarSlurper) Close() error {
	return ts.f.Close()
}

func (ts *tarSlurper) Read(p []byte) (int, error) {
	return ts.tr.Read(p)
}

func getReadCloser(tn, fn string) (io.ReadCloser, error) {
	pf, err := os.Open(tn)
	if err != nil {
		return nil, errors.Wrap(err, "opening pkg file")
	}
	tr := tar.NewReader(pf)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, errors.Wrap(err, "tar traversal")
		}
		if hdr.Name == fn {
			return &tarSlurper{pf, tr}, nil
		}
	}
	return nil, errors.Errorf("%q not found", fn)
}
