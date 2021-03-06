// Package shell implements the shell command.
package shell

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	isatty "github.com/mattn/go-isatty"
	"github.com/spf13/cobra"
	"github.com/taskcluster/taskcluster-cli/cmds/root"
	"github.com/taskcluster/taskcluster-cli/config"
	v1client "github.com/taskcluster/taskcluster-cli/pkg/docker-exec-ws"
	tcclient "github.com/taskcluster/taskcluster-client-go"
	"github.com/taskcluster/taskcluster-client-go/queue"
	"github.com/taskcluster/taskcluster-worker/engines"
	v2client "github.com/taskcluster/taskcluster-worker/plugins/interactive/shellclient"
	"github.com/taskcluster/taskcluster-worker/runtime/ioext"
)

var (
	// Command is the root of the shell sub-tree.
	Command = &cobra.Command{
		Use:   "shell <taskId> [-- command to execute]",
		Short: "Connect to the shell of a running interactive task.",
		RunE:  Execute,
	}
)

func init() {
	root.Command.AddCommand(Command)
}

// Execute runs the shell.
func Execute(cmd *cobra.Command, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("%s expects argument <taskId>", cmd.Name())
	}

	taskID := args[0]

	var creds *tcclient.Credentials
	if config.Credentials != nil {
		creds = config.Credentials.ToClientCredentials()
	}

	q := queue.New(creds)

	err := checkTask(q, taskID)
	if err != nil {
		return err
	}

	// At this point we know we have a valid task with interactivity.
	sURL, err := q.GetLatestArtifact_SignedURL(taskID, "private/docker-worker/shell.html", 1*time.Minute)
	if err != nil {
		return err
	}

	// client is an HTTP client that doesn't follow redirects.
	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	resp, err := client.Get(sURL.String())
	if err != nil {
		return err
	}
	redirectURL, err := resp.Location()
	if err != nil {
		return err
	}

	var sockURL *url.URL
	var shell engines.Shell
	tty := isatty.IsTerminal(os.Stdout.Fd())

	command := []string{}
	if len(args) > 1 {
		command = args[1:]
	}

	switch redirectURL.Query().Get("v") {
	case "1":
		if len(command) == 0 {
			// Default command for v1
			// as defined in https://github.com/taskcluster/taskcluster-tools/blob/edb67d523a8302313b1046448d7cbfda33c6d196/src/views/Shell/Shell.js#L7-L24
			command = []string{
				"sh", "-c",
				strings.Join([]string{
					"if [ -f \"/etc/taskcluster-motd\" ]; then cat /etc/taskcluster-motd; fi;",
					"if [ -z \"$TERM\" ]; then export TERM=xterm; fi;",
					"if [ -z \"$HOME\" ]; then export HOME=/root; fi;",
					"if [ -z \"$USER\" ]; then export USER=root; fi;",
					"if [ -z \"$LOGNAME\" ]; then export LOGNAME=root; fi;",
					"if [ -z `which \"$SHELL\"` ]; then export SHELL=bash; fi;",
					"if [ -z `which \"$SHELL\"` ]; then export SHELL=sh; fi;",
					"if [ -z `which \"$SHELL\"` ]; then export SHELL=\"/.taskclusterutils/busybox sh\"; fi;",
					"SPAWN=\"$SHELL\";",
					"if [ \"$SHELL\" = \"bash\" ]; then SPAWN=\"bash -li\"; fi;",
					"if [ -f \"/bin/taskcluster-interactive-shell\" ]; then SPAWN=\"/bin/taskcluster-interactive-shell\"; fi;",
					"exec $SPAWN;",
				}, ""),
			}
		}
		sockURL, _ = url.Parse(redirectURL.Query().Get("socketUrl"))
		shell, err = v1client.Dial(sockURL.String(), command, tty)
		if err != nil {
			return fmt.Errorf("could not create the shell client: %v", err)
		}
	case "2":
		sockURL, _ = url.Parse(redirectURL.Query().Get("socketUrl"))
		shell, err = v2client.Dial(sockURL.String(), command, tty)
		if err != nil {
			return fmt.Errorf("could not create the shell client: %v", err)
		}
	default:
		return errors.New("unknown shell version")
	}

	// Switch terminal to raw mode
	cleanup := func() {}
	if tty {
		cleanup = setupRawTerminal(shell.SetSize)
	}

	// Connect pipes
	go ioext.CopyAndClose(shell.StdinPipe(), os.Stdin)
	go io.Copy(os.Stdout, shell.StdoutPipe())
	go io.Copy(os.Stderr, shell.StderrPipe())

	// Wait for shell to be done
	_, err = shell.Wait()

	// If we were in a tty we let's restore state
	cleanup()

	return err
}

// checkTask makes sure that the given task is interactive and that we can connect.
func checkTask(q *queue.Queue, taskID string) error {
	task, err := q.Task(taskID)
	if err != nil {
		return fmt.Errorf("could not get the definition of task %s: %v", taskID, err)
	}
	var payload map[string]json.RawMessage
	json.Unmarshal(task.Payload, &payload)
	if _, ok := payload["features"]; !ok {
		return fmt.Errorf("task %s was created without features.interactive", taskID)
	}
	var features map[string]json.RawMessage
	json.Unmarshal(payload["features"], &features)
	if _, ok := features["interactive"]; !ok {
		return fmt.Errorf("task %s was created without features.interactive", taskID)
	}
	var interactive bool
	json.Unmarshal(features["interactive"], &interactive)
	if !interactive {
		return fmt.Errorf("task %s was created without features.interactive = true", taskID)
	}

	s, err := q.Status(taskID)
	if err != nil {
		return fmt.Errorf("could not get the status of task %s: %v", taskID, err)
	}
	lastRunState := s.Status.Runs[len(s.Status.Runs)-1].State
	lastRunDeadline := time.Time(s.Status.Runs[len(s.Status.Runs)-1].Resolved).Add(15 * time.Minute)
	if !(lastRunState == "running" || (lastRunState == "completed" && lastRunDeadline.After(time.Now().UTC()))) {
		return fmt.Errorf("task %s is not running and was not completed in the last 15 minutes", taskID)
	}

	return nil
}
