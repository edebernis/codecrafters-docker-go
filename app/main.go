package main

import (
	"errors"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
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

// Usage: your_docker.sh run <image> <command> <arg1> <arg2> ...
func main() {
	command := os.Args[3]
	args := os.Args[4:len(os.Args)]

	dir, err := ioutil.TempDir("/tmp", "docker")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)
	chrootRoot := filepath.Join("/tmp", dir)

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

	if err := syscall.Chdir(chrootRoot); err != nil {
		panic(err)
	}
	if err := syscall.Chroot(chrootRoot); err != nil {
		panic(err)
	}

	cmd := exec.Command(command, args...)
	cmd.Stdin = nullReader{}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if ok := errors.As(err, &exitErr); ok {
			os.Exit(exitErr.ExitCode())
		}
	}
}
