package main

import (
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
	tag := "latest"
	if len(imageSplit) == 2 {
		tag = imageSplit[1]
	}
	url := fmt.Sprintf("https://registry.hub.docker.com/v2/library/%s/manifests/%s", imageSplit[0], tag)

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

	outPath := filepath.Join(rootDir, "layer.tar.gz")
	out, err := os.Create(outPath)
	defer os.Remove(outPath)

	if _, err := io.Copy(out, resp.Body); err != nil {
		return err
	}

	if err := out.Close(); err != nil {
		return err
	}

	cmd := exec.Command("tar", "-xhf", outPath, "-C", rootDir)
	cmd.Stdin = nullReader{}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return err
	}

	return nil
}

// Usage: your_docker.sh run <image> <command> <arg1> <arg2> ...
func main() {
	image := os.Args[2]
	command := os.Args[3]
	args := os.Args[4:len(os.Args)]

	chrootRoot, err := ioutil.TempDir("", "docker")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(chrootRoot)

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

	cmd := exec.Command(command, args...)
	cmd.Stdin = nullReader{}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Chroot:     chrootRoot,
		Cloneflags: syscall.CLONE_NEWPID,
	}

	if err := cmd.Run(); err != nil {
		fmt.Printf("%s\n", err.Error())

		var exitErr *exec.ExitError
		if ok := errors.As(err, &exitErr); ok {
			os.Exit(exitErr.ExitCode())
		}
	}
}
