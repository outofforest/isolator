package docker

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/outofforest/logger"
	"github.com/pkg/errors"
	"github.com/ridge/must"
	"go.uber.org/zap"

	"github.com/outofforest/isolator/lib/retry"
)

// Apply fetches image from docker registry and integrates it inside directory
func Apply(ctx context.Context, c *http.Client, image, tag string) error {
	log := logger.Get(ctx)

	var token string
	if err := retry.Do(ctx, 10, 5*time.Second, func() error {
		var err error
		token, err = authorize(ctx, c, image)
		return err
	}); err != nil {
		return err
	}

	var l []string
	if err := retry.Do(ctx, 10, 5*time.Second, func() error {
		var err error
		l, err = layers(ctx, c, token, image, tag)
		return err
	}); err != nil {
		return err
	}
	for _, digest := range l {
		digest := digest
		log.Info("Incrementing filesystem", zap.String("digest", digest))
		if err := retry.Do(ctx, 10, 10*time.Second, func() error {
			return increment(ctx, c, token, image, digest)
		}); err != nil {
			return err
		}
	}
	return nil
}

func authorize(ctx context.Context, c *http.Client, imageName string) (string, error) {
	resp, err := c.Do(must.HTTPRequest(http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("https://auth.docker.io/token?service=registry.docker.io&scope=repository:library/%s:pull", imageName), nil)))
	if err != nil {
		return "", retry.Retryable(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", retry.Retryable(errors.Errorf("unexpected response status: %d", resp.StatusCode))
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", retry.Retryable(err)
	}

	data := struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"` // nolint: tagliatelle
	}{}
	if err := json.Unmarshal(body, &data); err != nil {
		return "", retry.Retryable(err)
	}
	if data.Token != "" {
		return data.Token, nil
	}
	if data.AccessToken != "" {
		return data.AccessToken, nil
	}
	return "", retry.Retryable(errors.New("no token in response"))
}

func layers(ctx context.Context, c *http.Client, token string, image, tag string) ([]string, error) {
	req := must.HTTPRequest(http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("https://registry-1.docker.io/v2/library/%s/manifests/%s", image, tag), nil))
	req.Header.Add("Accept", "application/vnd.docker.distribution.manifest.v2+json")
	req.Header.Add("Authorization", "Bearer "+token)
	resp, err := c.Do(req)
	if err != nil {
		return nil, retry.Retryable(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, retry.Retryable(errors.Errorf("unexpected response status: %d", resp.StatusCode))
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, retry.Retryable(errors.WithStack(err))
	}

	data := struct {
		Layers []struct {
			Digest string `json:"digest"`
		} `json:"layers"`
	}{}

	if err := json.Unmarshal(body, &data); err != nil {
		return nil, retry.Retryable(err)
	}

	layers := make([]string, 0, len(data.Layers))
	for _, l := range data.Layers {
		layers = append(layers, l.Digest)
	}
	return layers, nil
}

func increment(ctx context.Context, c *http.Client, token, imageName, digest string) error {
	req := must.HTTPRequest(http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("https://registry-1.docker.io/v2/library/%s/blobs/%s", imageName, digest), nil))
	req.Header.Add("Authorization", "Bearer "+token)
	resp, err := c.Do(req)
	if err != nil {
		return retry.Retryable(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return retry.Retryable(errors.Errorf("unexpected response status: %d", resp.StatusCode))
	}

	hasher := sha256.New()
	gr, err := gzip.NewReader(io.TeeReader(resp.Body, hasher))
	if err != nil {
		return retry.Retryable(errors.WithStack(err))
	}
	defer gr.Close()

	tr := tar.NewReader(gr)
	del := map[string]bool{}
	added := map[string]bool{}
loop:
	for {
		header, err := tr.Next()
		switch {
		case err == io.EOF:
			break loop
		case err != nil:
			return retry.Retryable(err)
		case header == nil:
			continue
		}
		if err := os.RemoveAll(header.Name); err != nil && !os.IsNotExist(err) {
			return errors.WithStack(err)
		}
		// We take mode from header.FileInfo().Mode(), not from header.Mode because they may be in different formats (meaning of bits may be different).
		// header.FileInfo().Mode() returns compatible value.
		mode := header.FileInfo().Mode()

		switch {
		case filepath.Base(header.Name) == ".wh..wh..plnk":
			// just ignore this
			continue
		case filepath.Base(header.Name) == ".wh..wh..opq":
			// It means that content in this directory created by earlier layers should not be visible,
			// so content created earlier should be deleted
			dir := filepath.Dir(header.Name)
			files, err := os.ReadDir(dir)
			if err != nil {
				return errors.WithStack(err)
			}
			for _, f := range files {
				toDelete := filepath.Join(dir, f.Name())
				if added[toDelete] {
					continue
				}
				if err := os.RemoveAll(toDelete); err != nil {
					return errors.WithStack(err)
				}
			}
			continue
		case strings.HasPrefix(filepath.Base(header.Name), ".wh."):
			// delete or mark to delete corresponding file
			toDelete := filepath.Join(filepath.Dir(header.Name), strings.TrimPrefix(filepath.Base(header.Name), ".wh."))
			delete(added, toDelete)
			if err := os.RemoveAll(toDelete); err != nil {
				if os.IsNotExist(err) {
					del[toDelete] = true
					continue
				}
				return errors.WithStack(err)
			}
			continue
		case del[header.Name]:
			delete(del, header.Name)
			delete(added, header.Name)
			continue
		case header.Typeflag == tar.TypeDir:
			if err := os.MkdirAll(header.Name, mode); err != nil {
				return errors.WithStack(err)
			}
		case header.Typeflag == tar.TypeReg:
			f, err := os.OpenFile(header.Name, os.O_CREATE|os.O_WRONLY, mode)
			if err != nil {
				return errors.WithStack(err)
			}
			_, err = io.Copy(f, tr)
			_ = f.Close()
			if err != nil {
				return errors.WithStack(err)
			}
		case header.Typeflag == tar.TypeSymlink:
			if err := os.Symlink(header.Linkname, header.Name); err != nil {
				return errors.WithStack(err)
			}
		case header.Typeflag == tar.TypeLink:
			// linked file may not exist yet, so let's create it - i will be overwritten later
			f, err := os.OpenFile(header.Linkname, os.O_CREATE|os.O_EXCL, mode)
			if err != nil {
				if !os.IsExist(err) {
					return errors.WithStack(err)
				}
			} else {
				_ = f.Close()
			}
			if err := os.Link(header.Linkname, header.Name); err != nil {
				return errors.WithStack(err)
			}
		default:
			return errors.Errorf("unsupported file type: %d", header.Typeflag)
		}

		added[header.Name] = true
		if err := os.Lchown(header.Name, header.Uid, header.Gid); err != nil {
			return errors.WithStack(err)
		}

		// Unless CAP_FSETID capability is set for the process every operation modifying the file/dir will reset
		// setuid, setgid nd sticky bits. After saving those files/dirs the mode has to be set once again to set those bits.
		// This has to be the last operation on the file/dir.
		// On linux mode is not supported for symlinks, mode is always taken from target location.
		if header.Typeflag != tar.TypeSymlink {
			if err := os.Chmod(header.Name, mode); err != nil {
				return errors.WithStack(err)
			}
		}
	}

	computedDigest := "sha256:" + hex.EncodeToString(hasher.Sum(nil))
	if computedDigest != digest {
		return retry.Retryable(errors.Errorf("digest doesn't match, expected: %s, got: %s", digest, computedDigest))
	}
	return nil
}
