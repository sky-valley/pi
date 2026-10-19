//go:build android || pienv_android

package daemon

// onAndroid: the daemon runs on Android (Termux). Go links android/amd64
// only externally, which needs cgo, so that target is built as linux/amd64
// with the pienv_android tag; android/arm64 builds as GOOS=android.
const onAndroid = true
