package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	cubesandbox "github.com/tencentcloud/CubeSandbox/sdk/go"
)

const (
	probePort = uint16(49983)
	image     = "ghcr.io/tencentcloud/cubesandbox-base:2026.16"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	client := cubesandbox.NewClient(cubesandbox.NewConfigFromEnv())
	defer client.Close()

	cpu := uint32(1000)
	memory := uint32(1024)
	probe := probePort
	job, err := client.BuildTemplate(ctx, cubesandbox.BuildTemplateOptions{
		Image:        imageFromEnv(),
		Name:         "cube-envd-verify",
		InstanceType: "cubebox",
		ExposedPorts: []uint16{probePort},
		ProbePort:    &probe,
		ProbePath:    "/health",
		CPU:          &cpu,
		Memory:       &memory,
	})
	if err != nil {
		fail("BuildTemplate", err)
	}
	printJSON("BuildTemplate", job)
	if job.TemplateID == "" || job.JobID == "" {
		fail("BuildTemplate", fmt.Errorf("response did not contain templateID and jobID"))
	}

	status, err := waitForBuild(ctx, client, job.TemplateID, job.JobID)
	if err != nil {
		fail("GetTemplateBuildStatus", err)
	}
	printJSON("GetTemplateBuildStatus", status)

	sandbox, err := client.Create(ctx, cubesandbox.CreateOptions{
		TemplateID: job.TemplateID,
		Timeout:    cubesandbox.DurationPtr(3 * time.Minute),
	})
	if err != nil {
		fail("Create", err)
	}
	printJSON("Create", sandbox)
	if sandbox.SandboxID == "" || sandbox.EnvdVersion == "" {
		fail("Create", fmt.Errorf("response did not contain sandboxID and envdVersion"))
	}
	defer killSandbox(sandbox)

	command, err := sandbox.Commands().Run(ctx, "echo -n cube-envd-template; whoami", cubesandbox.CommandOptions{})
	if err != nil {
		fail("Commands", err)
	}
	printJSON("Commands", command)
	if command.ExitCode != 0 || !strings.HasPrefix(command.Stdout, "cube-envd-template") || strings.TrimSpace(strings.TrimPrefix(command.Stdout, "cube-envd-template")) == "" {
		fail("Commands", fmt.Errorf("unexpected command result: %#v", command))
	}

	path := fmt.Sprintf("/tmp/cube-envd-template-validation-%d.txt", time.Now().UnixNano())
	defer sandbox.Files().Remove(context.Background(), path)
	want := "hello from cube-envd template"
	if err := sandbox.Files().Write(ctx, path, []byte(want)); err != nil {
		fail("Files.Write", err)
	}
	got, err := sandbox.Files().Read(ctx, path)
	if err != nil {
		fail("Files.Read", err)
	}
	printJSON("Files", map[string]string{"path": path, "content": got})
	if got != want {
		fail("Files", fmt.Errorf("read content %q, want %q", got, want))
	}

	fmt.Println("PASS: template ready, sandbox create, commands, and files")
}

func imageFromEnv() string {
	if value := strings.TrimSpace(os.Getenv("CUBE_ENVD_BASE_IMAGE")); value != "" {
		return value
	}
	return image
}

func waitForBuild(ctx context.Context, client *cubesandbox.Client, templateID, jobID string) (*cubesandbox.TemplateBuildStatus, error) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		status, err := client.GetTemplateBuildStatus(ctx, templateID, jobID)
		if err != nil {
			return nil, err
		}
		switch strings.ToLower(status.Status) {
		case "success", "ready", "succeeded", "completed":
			return status, nil
		case "error", "failed", "failure":
			return nil, fmt.Errorf("build failed: %s", status.Message)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

func killSandbox(sandbox *cubesandbox.Sandbox) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := sandbox.Kill(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to kill sandbox %s: %v\n", sandbox.SandboxID, err)
	}
}

func printJSON(name string, value any) {
	raw, err := json.Marshal(value)
	if err != nil {
		fail(name, err)
	}
	fmt.Printf("%s: %s\n", name, raw)
}

func fail(step string, err error) {
	fatal := fmt.Errorf("%s failed: %w", step, err)
	fmt.Fprintln(os.Stderr, fatal)
	os.Exit(1)
}
