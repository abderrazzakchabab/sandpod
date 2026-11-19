package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const (
	containerHome      = "/home/agent"
	containerWorkspace = "/home/agent/workspace"
	containerUID       = 1000
	containerGID       = 1000
)

// podman runs a podman command with inherited stdio.
func podman(args ...string) error {
	return podmanIO(nil, args...)
}

func podmanIO(stdin *string, args ...string) error {
	if verbose {
		fmt.Fprintln(os.Stderr, "+ podman", strings.Join(args, " "))
	}
	cmd := exec.Command("podman", args...)
	if stdin != nil {
		cmd.Stdin = strings.NewReader(*stdin)
	} else {
		cmd.Stdin = os.Stdin
	}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// podmanOutput runs a podman command and returns trimmed stdout.
func podmanOutput(args ...string) (string, error) {
	if verbose {
		fmt.Fprintln(os.Stderr, "+ podman", strings.Join(args, " "))
	}
	var out, errb bytes.Buffer
	cmd := exec.Command("podman", args...)
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("podman %s: %w: %s", args[0], err, strings.TrimSpace(errb.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

func requirePodman() error {
	if _, err := exec.LookPath("podman"); err != nil {
		return fmt.Errorf("podman not found on PATH; install it first (https://podman.io/docs/installation)")
	}
	return nil
}

func imageName(a Agent) string { return "localhost/podagent-" + a.Name + ":latest" }

func imageExists(name string) bool {
	return exec.Command("podman", "image", "exists", name).Run() == nil
}

// buildImage builds the agent image from the embedded Containerfile.
func buildImage(a Agent, noCache bool) error {
	dir, err := os.MkdirTemp("", "podagent-build-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	file := filepath.Join(dir, "Containerfile")
	if err := os.WriteFile(file, []byte(containerfile(a)), 0o644); err != nil {
		return err
	}
	args := []string{"build", "-t", imageName(a), "-f", file,
		"--build-arg", fmt.Sprintf("AGENT_UID=%d", containerUID),
		"--build-arg", fmt.Sprintf("AGENT_GID=%d", containerGID)}
	if noCache {
		args = append(args, "--no-cache")
	}
	fmt.Fprintf(os.Stderr, "podagent: building %s\n", imageName(a))
	return podman(append(args, dir)...)
}

func containerExists(name string) bool {
	return exec.Command("podman", "container", "exists", name).Run() == nil
}

func containerRunning(name string) bool {
	out, err := podmanOutput("inspect", "-f", "{{.State.Running}}", name)
	return err == nil && out == "true"
}

// Mount is a host bind mount.
type Mount struct {
	Host, Container string
	ReadOnly        bool
	// NoRelabel skips the SELinux `z` relabel, for host paths outside the
	// repo (like ~/.claude) whose labels we must not change.
	NoRelabel bool
}

func (m Mount) flag() string {
	var opts []string
	if m.ReadOnly {
		opts = append(opts, "ro")
	}
	if !m.NoRelabel {
		opts = append(opts, "z")
	}
	if len(opts) == 0 {
		return m.Host + ":" + m.Container
	}
	return fmt.Sprintf("%s:%s:%s", m.Host, m.Container, strings.Join(opts, ","))
}

// ContainerSpec is everything needed to create the long-lived container.
type ContainerSpec struct {
	Name   string
	Image  string
	Mounts []Mount
	Env    map[string]string
	Labels map[string]string
	Ports  []string
	// Volumes are raw `-v` values for podman named volumes.
	Volumes []string
}

// createContainer starts a detached container that sleeps forever. The host
// user maps to the image's agent UID/GID (--userns=keep-id), so files the
// agent writes in the workspace stay owned by the host user.
func createContainer(s ContainerSpec) error {
	args := []string{"run", "-d", "--name", s.Name,
		fmt.Sprintf("--userns=keep-id:uid=%d,gid=%d", containerUID, containerGID),
		"--user", fmt.Sprintf("%d:%d", containerUID, containerGID),
		"--workdir", containerWorkspace,
		"--hostname", "podagent",
		"--label", "podagent=1",
	}
	for k, v := range s.Labels {
		args = append(args, "--label", k+"="+v)
	}
	unlabeled := false
	for _, m := range s.Mounts {
		args = append(args, "-v", m.flag())
		unlabeled = unlabeled || m.NoRelabel
	}
	if unlabeled {
		// Unrelabeled host paths are unreadable under SELinux confinement.
		args = append(args, "--security-opt", "label=disable")
	}
	for _, k := range sortedKeys(s.Env) {
		// `-e KEY` (no value) reads from podman's own environment, which
		// keeps secrets off the command line and out of `ps`.
		args = append(args, "-e", k)
	}
	for _, v := range s.Volumes {
		args = append(args, "-v", v)
	}
	for _, p := range s.Ports {
		args = append(args, "-p", p)
	}
	args = append(args, s.Image)

	if verbose {
		fmt.Fprintln(os.Stderr, "+ podman", strings.Join(args, " "))
	}
	cmd := exec.Command("podman", args...)
	cmd.Env = os.Environ()
	for k, v := range s.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	var errb bytes.Buffer
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("podman run: %w: %s", err, strings.TrimSpace(errb.String()))
	}
	return nil
}

// execOpts controls a `podman exec` into the container.
type execOpts struct {
	TTY   bool
	Stdin *string
	Dir   string
}

func containerExec(name string, o execOpts, command ...string) error {
	args := []string{"exec"}
	if o.TTY {
		args = append(args, "-it")
	} else if o.Stdin != nil {
		args = append(args, "-i")
	}
	dir := o.Dir
	if dir == "" {
		dir = containerWorkspace
	}
	args = append(args, "-w", dir, name)
	args = append(args, command...)
	return podmanIO(o.Stdin, args...)
}

// containerShellOutput runs a bash script in the container and returns
// stdout. Extra args become $0, $1, ... so values are never spliced into the
// script text.
func containerShellOutput(name, dir, script string, args ...string) (string, error) {
	return podmanOutput(append([]string{"exec", "-w", dir, name, "bash", "-lc", script}, args...)...)
}
