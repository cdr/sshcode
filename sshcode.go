package main

import (
	"context"
	"fmt"
	"math/rand"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/pkg/browser"
	"go.coder.com/flog"
	"golang.org/x/xerrors"
)

const codeServerPath = "~/.cache/sshcode/sshcode-server"

type options struct {
	skipSync   bool
	syncBack   bool
	noOpen     bool
	bindAddr   string
	remotePort string
	sshFlags   string
}

func sshCode(host, dir string, o options) error {
	flog.Info("ensuring code-server is updated...")

	host, extraSSHFlags, err := parseHost(host)
	if err != nil {
		return xerrors.Errorf("failed to parse host IP: %w", err)
	}
	if extraSSHFlags != "" {
		o.sshFlags = strings.Join([]string{extraSSHFlags, o.sshFlags}, " ")
	}

	o.bindAddr, err = parseBindAddr(o.bindAddr)
	if err != nil {
		return xerrors.Errorf("failed to parse bind address: %w", err)
	}

	if o.remotePort == "" {
		o.remotePort, err = randomPort()
	}
	if err != nil {
		return xerrors.Errorf("failed to find available remote port: %w", err)
	}

	dlScript := downloadScript(codeServerPath)

	// Downloads the latest code-server and allows it to be executed.
	sshCmdStr := fmt.Sprintf("ssh %v %v /bin/bash", o.sshFlags, host)

	sshCmd := exec.Command("sh", "-c", sshCmdStr)
	sshCmd.Stdout = os.Stdout
	sshCmd.Stderr = os.Stderr
	sshCmd.Stdin = strings.NewReader(dlScript)
	err = sshCmd.Run()
	if err != nil {
		return xerrors.Errorf("failed to update code-server: \n---ssh cmd---\n%s\n---download script---\n%s: %w",
			sshCmdStr,
			dlScript,
			err,
		)
	}

	if !o.skipSync {
		start := time.Now()
		flog.Info("syncing settings")
		err = syncUserSettings(o.sshFlags, host, false)
		if err != nil {
			return xerrors.Errorf("failed to sync settings: %w", err)
		}

		flog.Info("synced settings in %s", time.Since(start))

		flog.Info("syncing extensions")
		err = syncExtensions(o.sshFlags, host, false)
		if err != nil {
			return xerrors.Errorf("failed to sync extensions: %w", err)
		}
		flog.Info("synced extensions in %s", time.Since(start))
	}

	flog.Info("starting code-server...")

	flog.Info("Tunneling remote port %v to %v", o.remotePort, o.bindAddr)

	sshCmdStr =
		fmt.Sprintf("ssh -tt -q -L %v:localhost:%v %v %v 'cd %v; %v --host 127.0.0.1 --allow-http --no-auth --port=%v'",
			o.bindAddr, o.remotePort, o.sshFlags, host, dir, codeServerPath, o.remotePort,
		)

	// Starts code-server and forwards the remote port.
	sshCmd = exec.Command("sh", "-c", sshCmdStr)
	sshCmd.Stdin = os.Stdin
	sshCmd.Stdout = os.Stdout
	sshCmd.Stderr = os.Stderr
	err = sshCmd.Start()
	if err != nil {
		return xerrors.Errorf("failed to start code-server: %w", err)
	}

	url := fmt.Sprintf("http://%s", o.bindAddr)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	client := http.Client{
		Timeout: time.Second * 3,
	}
	for {
		if ctx.Err() != nil {
			return xerrors.Errorf("code-server didn't start in time: %w", ctx.Err())
		}
		// Waits for code-server to be available before opening the browser.
		resp, err := client.Get(url)
		if err != nil {
			continue
		}
		resp.Body.Close()
		break
	}

	ctx, cancel = context.WithCancel(context.Background())

	if !o.noOpen {
		openBrowser(url)
	}

	go func() {
		defer cancel()
		sshCmd.Wait()
	}()

	c := make(chan os.Signal)
	signal.Notify(c, os.Interrupt)

	select {
	case <-ctx.Done():
	case <-c:
	}

	if !o.syncBack || o.skipSync {
		flog.Info("shutting down")
		return nil
	}

	flog.Info("synchronizing VS Code back to local")

	err = syncExtensions(o.sshFlags, host, true)
	if err != nil {
		return xerrors.Errorf("failed to sync extensions back: %w", err)
	}

	err = syncUserSettings(o.sshFlags, host, true)
	if err != nil {
		return xerrors.Errorf("failed to sync user settings settings back: %w", err)
	}

	return nil
}

func parseBindAddr(bindAddr string) (string, error) {
	if bindAddr == "" {
		bindAddr = ":"
	}

	host, port, err := net.SplitHostPort(bindAddr)
	if err != nil {
		return "", err
	}

	if host == "" {
		host = "127.0.0.1"
	}

	if port == "" {
		port, err = randomPort()
	}
	if err != nil {
		return "", err
	}

	return net.JoinHostPort(host, port), nil
}

func openBrowser(url string) {
	var openCmd *exec.Cmd

	const (
		macPath = "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
		wslPath = "/mnt/c/Program Files (x86)/Google/Chrome/Application/chrome.exe"
	)

	switch {
	case commandExists("google-chrome"):
		openCmd = exec.Command("google-chrome", chromeOptions(url)...)
	case commandExists("google-chrome-stable"):
		openCmd = exec.Command("google-chrome-stable", chromeOptions(url)...)
	case commandExists("chromium"):
		openCmd = exec.Command("chromium", chromeOptions(url)...)
	case commandExists("chromium-browser"):
		openCmd = exec.Command("chromium-browser", chromeOptions(url)...)
	case pathExists(macPath):
		openCmd = exec.Command(macPath, chromeOptions(url)...)
	case pathExists(wslPath):
		openCmd = exec.Command(wslPath, chromeOptions(url)...)
	default:
		err := browser.OpenURL(url)
		if err != nil {
			flog.Error("failed to open browser: %v", err)
		}
		return
	}

	// We do not use CombinedOutput because if there is no chrome instance, this will block
	// and become the parent process instead of using an existing chrome instance.
	err := openCmd.Start()
	if err != nil {
		flog.Error("failed to open browser: %v", err)
	}
}

func chromeOptions(url string) []string {
	return []string{"--app=" + url, "--disable-extensions", "--disable-plugins", "--incognito"}
}

// Checks if a command exists locally.
func commandExists(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

func pathExists(name string) bool {
	_, err := os.Stat(name)
	return err == nil
}

// randomPort picks a random port to start code-server on.
func randomPort() (string, error) {
	const (
		minPort  = 1024
		maxPort  = 65535
		maxTries = 10
	)
	for i := 0; i < maxTries; i++ {
		port := rand.Intn(maxPort-minPort+1) + minPort
		l, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
		if err == nil {
			_ = l.Close()
			return strconv.Itoa(port), nil
		}
		flog.Info("port taken: %d", port)
	}

	return "", xerrors.Errorf("max number of tries exceeded: %d", maxTries)
}

func syncUserSettings(sshFlags string, host string, back bool) error {
	localConfDir, err := configDir()
	if err != nil {
		return err
	}

	err = ensureDir(localConfDir)
	if err != nil {
		return err
	}

	const remoteSettingsDir = "~/.local/share/code-server/User/"

	var (
		src  = localConfDir + "/"
		dest = host + ":" + remoteSettingsDir
	)

	if back {
		dest, src = src, dest
	}

	// Append "/" to have rsync copy the contents of the dir.
	return rsync(src, dest, sshFlags, "workspaceStorage", "logs", "CachedData")
}

func syncExtensions(sshFlags string, host string, back bool) error {
	localExtensionsDir, err := extensionsDir()
	if err != nil {
		return err
	}

	err = ensureDir(localExtensionsDir)
	if err != nil {
		return err
	}

	const remoteExtensionsDir = "~/.local/share/code-server/extensions/"

	var (
		src  = localExtensionsDir + "/"
		dest = host + ":" + remoteExtensionsDir
	)
	if back {
		dest, src = src, dest
	}

	return rsync(src, dest, sshFlags)
}

func rsync(src string, dest string, sshFlags string, excludePaths ...string) error {
	excludeFlags := make([]string, len(excludePaths))
	for i, path := range excludePaths {
		excludeFlags[i] = "--exclude=" + path
	}

	cmd := exec.Command("rsync", append(excludeFlags, "-azvr",
		"-e", "ssh "+sshFlags,
		// Only update newer directories, and sync times
		// to keep things simple.
		"-u", "--times",
		// This is more unsafe, but it's obnoxious having to enter VS Code
		// locally in order to properly delete an extension.
		"--delete",
		"--copy-unsafe-links",
		src, dest,
	)...,
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	if err != nil {
		return xerrors.Errorf("failed to rsync '%s' to '%s': %w", src, dest, err)
	}

	return nil
}

func downloadScript(codeServerPath string) string {
	return fmt.Sprintf(
		`set -euxo pipefail || exit 1

pkill -f %v || true
mkdir -p ~/.local/share/code-server %v
cd %v
wget -N https://codesrv-ci.cdr.sh/latest-linux
[ -f %v ] && rm %v
ln latest-linux %v
chmod +x %v`,
		codeServerPath,
		filepath.Dir(codeServerPath),
		filepath.Dir(codeServerPath),
		codeServerPath,
		codeServerPath,
		codeServerPath,
		codeServerPath,
	)
}

// ensureDir creates a directory if it does not exist.
func ensureDir(path string) error {
	_, err := os.Stat(path)
	if os.IsNotExist(err) {
		err = os.MkdirAll(path, 0750)
	}

	if err != nil {
		return err
	}

	return nil
}

// parseHost parses the host argument. If 'gcp:' is prefixed to the
// host then a lookup is done using gcloud to determine the external IP and any
// additional SSH arguments that should be used for ssh commands. Otherwise, host
// is returned.
func parseHost(host string) (parsedHost string, additionalFlags string, err error) {
	host = strings.TrimSpace(host)
	switch {
	case strings.HasPrefix(host, "gcp:"):
		instance := strings.TrimPrefix(host, "gcp:")
		return parseGCPSSHCmd(instance)
	default:
		return host, "", nil
	}
}

// parseGCPSSHCmd parses the IP address and flags used by 'gcloud' when
// ssh'ing to an instance.
func parseGCPSSHCmd(instance string) (ip, sshFlags string, err error) {
	dryRunCmd := fmt.Sprintf("gcloud compute ssh --dry-run %v", instance)

	out, err := exec.Command("sh", "-c", dryRunCmd).CombinedOutput()
	if err != nil {
		return "", "", xerrors.Errorf("%s: %w", out, err)
	}

	toks := strings.Split(string(out), " ")
	if len(toks) < 2 {
		return "", "", xerrors.Errorf("unexpected output for '%v' command, %s", dryRunCmd, out)
	}

	// Slice off the '/usr/bin/ssh' prefix and the '<user>@<ip>' suffix.
	sshFlags = strings.Join(toks[1:len(toks)-1], " ")

	// E.g. foo@1.2.3.4.
	userIP := toks[len(toks)-1]
	toks = strings.Split(userIP, "@")
	// Assume the '<user>@' is missing.
	if len(toks) < 2 {
		ip = strings.TrimSpace(toks[0])
	} else {
		ip = strings.TrimSpace(toks[1])
	}

	if net.ParseIP(ip) == nil {
		return "", "", xerrors.Errorf("parsed invalid ip address %v", ip)
	}

	return ip, sshFlags, nil
}
