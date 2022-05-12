package main

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

type nullReader struct{}

func (nullReader) Read(p []byte) (n int, err error) { return len(p), nil }

func copy(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	info, err := os.Stat(src)
	if err != nil {
		return err
	}

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer func() { _ = out.Close() }()

	_, err = io.Copy(out, in)
	if err != nil {
		return err
	}

	return os.Chmod(dst, info.Mode())
}

type registryTokenSvcResponse struct {
	Token string `json:"token,omitempty"`
}

func registryLogin(image string) (string, error) {
	imageSplit := strings.Split(image, ":")
	url := fmt.Sprintf("https://auth.docker.io/token?service=registry.docker.io&scope=repository:library/%s:pull", imageSplit[0])

	resp, err := http.DefaultClient.Get(url)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("failed to get docker registry token. Status code: %d", resp.StatusCode)
	}

	var response registryTokenSvcResponse
	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		return "", err
	}

	return response.Token, nil
}

type manifestResponse struct {
	Layers []layer `json:"layers,omitempty"`
}

type layer struct {
	MediaType string `json:"mediaType,omitempty"`
	Size      int64  `json:"size,omitempty"`
	Digest    string `json:"digest,omitempty"`
}

func fetchManifest(token, image string) (manifestResponse, error) {
	imageSplit := strings.Split(image, ":")
	url := fmt.Sprintf("https://registry.hub.docker.com/v2/library/%s/manifests/%s", imageSplit[0], imageSplit[1])

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return manifestResponse{}, err
	}

	req.Header.Add("Authorization", fmt.Sprintf("Bearer %s", token))
	req.Header.Add("Accept", "application/vnd.docker.distribution.manifest.v2+json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return manifestResponse{}, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return manifestResponse{}, fmt.Errorf("failed to get image manifest. Status code: %d", resp.StatusCode)
	}

	var response manifestResponse
	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		return manifestResponse{}, err
	}

	return response, nil
}

func extractLayer(token, image, digest, rootDir string) error {
	imageSplit := strings.Split(image, ":")
	url := fmt.Sprintf("https://registry.hub.docker.com/v2/library/%s/blobs/%s", imageSplit[0], digest)

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return err
	}

	req.Header.Add("Authorization", fmt.Sprintf("Bearer %s", token))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("failed to get image manifest. Status code: %d", resp.StatusCode)
	}

	gzr, err := gzip.NewReader(resp.Body)
	if err != nil {
		return err
	}
	defer gzr.Close()

	tr := tar.NewReader(gzr)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		target := filepath.Join(rootDir, header.Name)

		switch header.Typeflag {
		case tar.TypeDir:
			if _, err := os.Stat(target); err != nil {
				if err := os.MkdirAll(target, 0755); err != nil {
					return err
				}
			}
		case tar.TypeReg:
			f, err := os.OpenFile(target, os.O_CREATE|os.O_RDWR, os.FileMode(header.Mode))
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				return err
			}
			f.Close()
		}
	}

	return nil
}

// Usage: your_docker.sh run <image> <command> <arg1> <arg2> ...
func main() {
	image := os.Args[2]
	command := os.Args[3]
	args := os.Args[4:len(os.Args)]

	dir, err := ioutil.TempDir("/tmp", "docker")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)
	chrootRoot := filepath.Join("/tmp", dir)

	token, err := registryLogin(image)
	if err != nil {
		panic(err)
	}

	manifest, err := fetchManifest(token, image)
	if err != nil {
		panic(err)
	}

	for _, layer := range manifest.Layers {
		if err := extractLayer(token, image, layer.Digest, chrootRoot); err != nil {
			panic(err)
		}
	}

	commandDir := filepath.Dir(command)
	commandName := filepath.Base(command)
	chrootCommandDir := filepath.Join(chrootRoot, commandDir)
	chrootCommand := filepath.Join(chrootCommandDir, commandName)

	if err := os.MkdirAll(chrootCommandDir, os.ModePerm); err != nil {
		panic(err)
	}

	if err := copy(command, chrootCommand); err != nil {
		panic(err)
	}

	cmd := exec.Command(command, args...)
	cmd.Stdin = nullReader{}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Chroot:     chrootRoot,
		Cloneflags: syscall.CLONE_NEWPID,
	}

	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if ok := errors.As(err, &exitErr); ok {
			os.Exit(exitErr.ExitCode())
		}
	}
}
