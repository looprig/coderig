module github.com/looprig/swe

go 1.26.4

require (
	github.com/looprig/cli v0.2.0
	github.com/looprig/core v0.1.0
	github.com/looprig/fsstore v0.1.0
	github.com/looprig/harness v0.5.2
	github.com/looprig/inference v0.1.0
	github.com/looprig/llm v0.1.0
	github.com/looprig/sandbox v0.0.0
)

require (
	charm.land/bubbles/v2 v2.1.0 // indirect
	charm.land/bubbletea/v2 v2.0.7 // indirect
	charm.land/glamour/v2 v2.0.1 // indirect
	charm.land/lipgloss/v2 v2.0.4 // indirect
	github.com/alecthomas/chroma/v2 v2.20.0 // indirect
	github.com/atotto/clipboard v0.1.4 // indirect
	github.com/aymerick/douceur v0.2.0 // indirect
	github.com/charmbracelet/colorprofile v0.4.3 // indirect
	github.com/charmbracelet/ultraviolet v0.0.0-20260525132238-948f4557a654 // indirect
	github.com/charmbracelet/x/ansi v0.11.7 // indirect
	github.com/charmbracelet/x/exp/slice v0.0.0-20250327172914-2fdc97757edf // indirect
	github.com/charmbracelet/x/term v0.2.2 // indirect
	github.com/charmbracelet/x/termios v0.1.1 // indirect
	github.com/charmbracelet/x/windows v0.2.2 // indirect
	github.com/clipperhouse/displaywidth v0.11.0 // indirect
	github.com/clipperhouse/uax29/v2 v2.7.0 // indirect
	github.com/dlclark/regexp2 v1.11.5 // indirect
	github.com/google/go-cmp v0.7.0 // indirect
	github.com/google/go-tdx-guest v0.3.1 // indirect
	github.com/google/logger v1.1.1 // indirect
	github.com/google/nftables v0.3.0 // indirect
	github.com/gorilla/css v1.0.1 // indirect
	github.com/landlock-lsm/go-landlock v0.9.0 // indirect
	github.com/looprig/storage v0.1.0 // indirect
	github.com/lucasb-eyer/go-colorful v1.4.0 // indirect
	github.com/mattn/go-runewidth v0.0.23 // indirect
	github.com/mdlayher/netlink v1.7.3-0.20250113171957-fbb4dce95f42 // indirect
	github.com/mdlayher/socket v0.5.0 // indirect
	github.com/microcosm-cc/bluemonday v1.0.27 // indirect
	github.com/muesli/cancelreader v0.2.2 // indirect
	github.com/rivo/uniseg v0.4.7 // indirect
	github.com/xo/terminfo v0.0.0-20220910002029-abceb7e1c41e // indirect
	github.com/yuin/goldmark v1.8.2 // indirect
	github.com/yuin/goldmark-emoji v1.0.6 // indirect
	go.uber.org/multierr v1.11.0 // indirect
	golang.org/x/crypto v0.52.0 // indirect
	golang.org/x/net v0.55.0 // indirect
	golang.org/x/sync v0.20.0 // indirect
	golang.org/x/sys v0.45.0 // indirect
	golang.org/x/text v0.37.0 // indirect
	google.golang.org/protobuf v1.36.11 // indirect
	kernel.org/pub/linux/libs/security/libcap/psx v1.2.77 // indirect
)

// Local, unpublished/private looprig modules resolved via sibling checkouts under
// /Users/ipotter/code/looprig. These modules are private (SSH auth) and there is no git
// insteadOf config, so `go` cannot fetch them over HTTPS — the local replaces are load-bearing
// for every offline build (GOWORK=off GOPROXY=off GOPRIVATE=github.com/looprig/*).
replace (
	github.com/looprig/cli => ../cli
	github.com/looprig/core => ../core
	github.com/looprig/fsstore => ../fsstore
	github.com/looprig/harness => ../harness
	github.com/looprig/inference => ../inference
	github.com/looprig/llm => ../llm
	github.com/looprig/sandbox => ../sandbox
	github.com/looprig/storage => ../storage
)
