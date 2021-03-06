package preflight

import (
	"bytes"
	"fmt"
	"io/ioutil"
	goos "os"
	"os/exec"
	"path/filepath"
	"strings"
	goTemplate "text/template"

	"github.com/Masterminds/semver"
	"github.com/code-ready/crc/pkg/crc/constants"
	"github.com/code-ready/crc/pkg/crc/logging"
	"github.com/code-ready/crc/pkg/crc/version"
	dl "github.com/code-ready/crc/pkg/download"
	"github.com/code-ready/crc/pkg/embed"
	"github.com/code-ready/crc/pkg/extract"
	"github.com/code-ready/crc/pkg/os"
	"github.com/pkg/errors"
	"howett.net/plist"
)

const (
	trayPlistTemplate = `<?xml version='1.0' encoding='UTF-8'?>
	<!DOCTYPE plist PUBLIC "-//Apple Computer//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
	<plist version='1.0'>
		<dict>
			<key>Label</key>
			<string>crc.tray</string>
			<key>ProgramArguments</key>
			<array>
				<string>{{ .BinaryPath }}</string>
			</array>
			<key>StandardOutPath</key>
			<string>{{ .StdOutFilePath }}</string>
			<key>Disabled</key>
			<false/>
			<key>RunAtLoad</key>
			<true/>
		</dict>
	</plist>`

	daemonPlistTemplate = `<?xml version='1.0' encoding='UTF-8'?>
	<!DOCTYPE plist PUBLIC "-//Apple Computer//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
	<plist version='1.0'>
		<dict>
			<key>Label</key>
			<string>crc.daemon</string>
			<key>ProgramArguments</key>
			<array>
				<string>{{ .BinaryPath }}</string>
				<string>daemon</string>
				<string>--log-level</string>
				<string>debug</string>
			</array>
			<key>StandardOutPath</key>
			<string>{{ .StdOutFilePath }}</string>
			<key>KeepAlive</key>
			<true/>
			<key>Disabled</key>
			<false/>
		</dict>
	</plist>`

	daemonAgentLabel = "crc.daemon"
	trayAgentLabel   = "crc.tray"
)

var (
	launchAgentsDir      = filepath.Join(constants.GetHomeDir(), "Library", "LaunchAgents")
	daemonPlistFilePath  = filepath.Join(launchAgentsDir, "crc.daemon.plist")
	trayPlistFilePath    = filepath.Join(launchAgentsDir, "crc.tray.plist")
	stdOutFilePathDaemon = filepath.Join(constants.CrcBaseDir, ".crcd-agent.log")
	stdOutFilePathTray   = filepath.Join(constants.CrcBaseDir, ".crct-agent.log")
)

type AgentConfig struct {
	BinaryPath     string
	StdOutFilePath string
}

type TrayVersion struct {
	ShortVersion string `plist:"CFBundleShortVersionString"`
}

func checkTrayExistsAndRunning() error {
	logging.Debug("Checking if daemon plist file exists")
	if !os.FileExists(daemonPlistFilePath) {
		return errors.New("Daemon plist file does not exist")
	}
	logging.Debug("Checking if crc agent running")
	if !agentRunning(daemonAgentLabel) {
		return errors.New("crc daemon is not running")
	}
	logging.Debug("Checking if tray plist file exists")
	if !os.FileExists(trayPlistFilePath) {
		return errors.New("Tray plist file does not exist")
	}
	logging.Debug("Checking if tray agent running")
	if !agentRunning(trayAgentLabel) {
		return errors.New("Tray is not running")
	}
	logging.Debug("Check if correct version of tray exists")
	if !checkTrayVersion() {
		return errors.New("cached tray version is older then expected")
	}
	return nil
}

func fixTrayExistsAndRunning() error {
	if err := ensureLaunchAgentsDirExists(); err != nil {
		return err
	}
	// get the tray app
	err := downloadOrExtractTrayApp()
	if err != nil {
		return err
	}
	currentExecutablePath, err := goos.Executable()
	if err != nil {
		return err
	}
	daemonConfig := AgentConfig{
		BinaryPath:     currentExecutablePath,
		StdOutFilePath: stdOutFilePathDaemon,
	}

	trayConfig := AgentConfig{
		BinaryPath:     filepath.Join(constants.CrcBinDir, constants.TrayBinaryName, "Contents", "MacOS", "CodeReady Containers"),
		StdOutFilePath: stdOutFilePathTray,
	}
	logging.Debug("Creating daemon plist")
	err = createPlist(daemonPlistTemplate, daemonConfig, daemonPlistFilePath)
	if err != nil {
		return err
	}
	logging.Debug("Creating tray plist")
	err = createPlist(trayPlistTemplate, trayConfig, trayPlistFilePath)
	if err != nil {
		return err
	}

	// load crc daemon
	err = launchctlLoadPlist(daemonPlistFilePath)
	if err != nil {
		return err
	}
	if !agentRunning(daemonAgentLabel) {
		if err = startAgent(daemonAgentLabel); err != nil {
			return err
		}
	}
	// load tray
	err = launchctlLoadPlist(trayPlistFilePath)
	if err != nil {
		return err
	}
	if !agentRunning(trayAgentLabel) {
		if err = startAgent(trayAgentLabel); err != nil {
			return err
		}
	}
	// restart tray and daemon agents
	err = restartAgent(daemonAgentLabel)
	if err != nil {
		return err
	}
	err = restartAgent(trayAgentLabel)
	if err != nil {
		return err
	}
	return nil
}

func createPlist(template string, config AgentConfig, plistPath string) error {
	var plistContent bytes.Buffer
	t, err := goTemplate.New("plist").Parse(template)
	if err != nil {
		return err
	}
	err = t.Execute(&plistContent, config)
	if err != nil {
		return err
	}
	err = ioutil.WriteFile(plistPath, plistContent.Bytes(), 0644)
	return err
}

func launchctlLoadPlist(plistFilePath string) error {
	_, err := exec.Command("launchctl", "load", plistFilePath).Output() // #nosec G204
	return err
}

func startAgent(label string) error {
	_, err := exec.Command("launchctl", "start", label).Output() // #nosec G204
	return err
}

func restartAgent(label string) error {
	_, err := exec.Command("launchctl", "stop", label).Output() // #nosec G204
	if err != nil {
		return err
	}
	_, err = exec.Command("launchctl", "start", label).Output() // #nosec G204
	return err
}

// check if a service (daemon,tray) is running
func agentRunning(label string) bool {
	// This command return a PID if the process
	// is running, otherwise returns "-"
	launchctlListCommand := `launchctl list | grep %s | awk '{print $1}'`
	cmd := fmt.Sprintf(launchctlListCommand, label)
	out, err := exec.Command("bash", "-c", cmd).Output() // #nosec G204
	if err != nil {
		return false
	}
	if strings.TrimSpace(string(out)) == "-" {
		return false
	}
	return true
}

func downloadOrExtractTrayApp() error {
	// Extract the tray and put it in the bin directory.
	tmpArchivePath, err := ioutil.TempDir("", "crc")
	if err != nil {
		logging.Error("Failed creating temporary directory for extracting tray")
		return err
	}
	defer func() {
		_ = goos.RemoveAll(tmpArchivePath)
	}()

	logging.Debug("Trying to extract tray from crc binary")
	err = embed.Extract(filepath.Base(constants.GetCrcTrayDownloadURL()), tmpArchivePath)
	if err != nil {
		logging.Debug("Downloading crc tray")
		_, err = dl.Download(constants.GetCrcTrayDownloadURL(), tmpArchivePath, 0600)
		if err != nil {
			return err
		}
	}
	archivePath := filepath.Join(tmpArchivePath, filepath.Base(constants.GetCrcTrayDownloadURL()))
	outputPath := constants.CrcBinDir
	err = goos.MkdirAll(outputPath, 0750)
	if err != nil && !goos.IsExist(err) {
		return errors.Wrap(err, "Cannot create the target directory.")
	}
	err = extract.Uncompress(archivePath, outputPath)
	if err != nil {
		return errors.Wrapf(err, "Cannot uncompress '%s'", archivePath)
	}
	return nil
}
func ensureLaunchAgentsDirExists() error {
	if err := goos.MkdirAll(launchAgentsDir, 0700); err != nil {
		return err
	}
	return nil
}

func getTrayVersion(trayAppPath string) (string, error) {
	var version TrayVersion
	f, err := ioutil.ReadFile(filepath.Join(trayAppPath, "Contents", "Info.plist")) // #nosec G304
	if err != nil {
		return "", err
	}
	decoder := plist.NewDecoder(bytes.NewReader(f))
	err = decoder.Decode(&version)
	if err != nil {
		return "", err
	}

	return version.ShortVersion, nil
}

func checkTrayVersion() bool {
	v, err := getTrayVersion(constants.TrayBinaryPath)
	if err != nil {
		logging.Error(err.Error())
		return false
	}
	currentVersion, err := semver.NewVersion(v)
	if err != nil {
		logging.Error(err.Error())
		return false
	}
	expectedVersion, err := semver.NewVersion(version.GetCRCTrayVersion())
	if err != nil {
		logging.Error(err.Error())
		return false
	}

	if expectedVersion.GreaterThan(currentVersion) {
		return false
	}
	return true
}
