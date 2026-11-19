package main

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
)

// attachedContainerURI builds the URI the VS Code Dev Containers extension
// uses for "Attach to Running Container".
func attachedContainerURI(container, folder string) string {
	cfg, _ := json.Marshal(map[string]string{"containerName": "/" + container})
	return fmt.Sprintf("vscode-remote://attached-container+%s%s", hex.EncodeToString(cfg), folder)
}

func openVSCode(container, folder string) error {
	bin := "code"
	if _, err := exec.LookPath(bin); err != nil {
		if _, err := exec.LookPath("code-insiders"); err == nil {
			bin = "code-insiders"
		} else {
			return fmt.Errorf("VS Code CLI `code` not on PATH. Attach manually: Dev Containers: Attach to Running Container… > %s", container)
		}
	}
	fmt.Fprintln(os.Stderr, `podagent: needs the "Dev Containers" extension with "dev.containers.dockerPath": "podman" in VS Code settings`)
	cmd := exec.Command(bin, "--folder-uri", attachedContainerURI(container, folder))
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}
