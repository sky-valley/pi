//go:build !linux

package daemon

const seccompExecVariable = "PI_ENV_TEST_SECCOMP_EXEC"

func execUnderSeccomp(string) {}
