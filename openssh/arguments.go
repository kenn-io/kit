package openssh

import (
	"strconv"
	"strings"
	"time"
)

// ConnectionOptions controls bounded noninteractive OpenSSH connections.
// ServerAliveCountMax must be positive; manager construction and
// MasterArguments reject custom option sets that omit it. Nonpositive timeout
// durations use a one-second minimum.
type ConnectionOptions struct {
	ConnectTimeout      time.Duration
	ServerAliveInterval time.Duration
	ServerAliveCountMax int
	TCPKeepAlive        bool
}

// DefaultConnectionOptions returns the fleet defaults shared by existing Kenn
// clients.
func DefaultConnectionOptions() ConnectionOptions {
	return ConnectionOptions{
		ConnectTimeout:      10 * time.Second,
		ServerAliveInterval: 15 * time.Second,
		ServerAliveCountMax: 3,
		TCPKeepAlive:        true,
	}
}

// MasterArguments builds a detached noninteractive ControlMaster invocation.
func MasterArguments(
	controlPath string,
	target Target,
	options ConnectionOptions,
) ([]string, error) {
	if err := ValidateTarget(target); err != nil {
		return nil, err
	}
	if err := validateConnectionOptions(options, target.String()); err != nil {
		return nil, err
	}
	literalPath, err := literalControlPath(controlPath)
	if err != nil {
		return nil, err
	}
	arguments := []string{
		"-MNf",
		"-o", "ControlMaster=yes",
		"-o", "ControlPersist=no",
		"-S", literalPath,
		"-o", "BatchMode=yes",
		"-o", "ConnectionAttempts=1",
	}
	arguments = append(arguments, connectionOptionArguments(options)...)
	destination, port := target.CommandDestination()
	if port != 0 {
		arguments = append(arguments, "-p", strconv.Itoa(port))
	}
	return append(arguments, "--", destination), nil
}

func validateConnectionOptions(options ConnectionOptions, destination string) error {
	if options.ServerAliveCountMax <= 0 {
		return &ConfigError{
			Destination: destination,
			Reason:      "ServerAliveCountMax must be positive",
		}
	}
	return nil
}

// ClientArguments makes connection sharing explicit. An empty control path is
// first-class and disables implicit socket discovery or master creation.
func ClientArguments(controlPath string) ([]string, error) {
	arguments := []string{
		"-o", "ControlMaster=no",
		"-o", "ControlPersist=no",
	}
	if controlPath == "" {
		return append(arguments, "-S", "none"), nil
	}
	literalPath, err := literalControlPath(controlPath)
	if err != nil {
		return nil, err
	}
	return append(arguments, "-S", literalPath), nil
}

func literalControlPath(path string) (string, error) {
	if strings.Contains(path, "${") {
		return "", &PathError{
			Path: path, Reason: "environment-variable expansion is not allowed",
		}
	}
	if path == "none" {
		path = "./none"
	}
	if strings.HasPrefix(path, "~") {
		path = "./" + path
	}
	return strings.ReplaceAll(path, "%", "%%"), nil
}

// CheckArguments builds an ssh -O check invocation.
func CheckArguments(controlPath string, target Target) ([]string, error) {
	if err := ValidateTarget(target); err != nil {
		return nil, err
	}
	arguments, err := ClientArguments(controlPath)
	if err != nil {
		return nil, err
	}
	destination, port := target.CommandDestination()
	arguments = append(arguments, "-O", "check")
	if port != 0 {
		arguments = append(arguments, "-p", strconv.Itoa(port))
	}
	return append(arguments, "--", destination), nil
}

// ExitArguments builds an ssh -O exit invocation.
func ExitArguments(controlPath string, target Target) ([]string, error) {
	if err := ValidateTarget(target); err != nil {
		return nil, err
	}
	arguments, err := ClientArguments(controlPath)
	if err != nil {
		return nil, err
	}
	destination, port := target.CommandDestination()
	arguments = append(arguments, "-O", "exit")
	if port != 0 {
		arguments = append(arguments, "-p", strconv.Itoa(port))
	}
	return append(arguments, "--", destination), nil
}

func connectionOptionArguments(options ConnectionOptions) []string {
	arguments := []string{
		"-o", "ConnectTimeout=" + durationSeconds(options.ConnectTimeout),
		"-o", "ServerAliveInterval=" + durationSeconds(options.ServerAliveInterval),
		"-o", "ServerAliveCountMax=" + strconv.Itoa(options.ServerAliveCountMax),
	}
	keepAlive := "no"
	if options.TCPKeepAlive {
		keepAlive = "yes"
	}
	return append(arguments, "-o", "TCPKeepAlive="+keepAlive)
}

func durationSeconds(value time.Duration) string {
	seconds := max(int(value/time.Second), 1)
	return strconv.Itoa(seconds)
}
