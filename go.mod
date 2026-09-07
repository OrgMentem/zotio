module zotio

// 1.26.5 fixes CVE-2026-42505 (ECH privacy leak in crypto/tls), and every
// outbound metadata provider call rides that stack. Patch-level go directive:
// an older 1.26.x is refused, and GOTOOLCHAIN=auto fetches 1.26.5.
go 1.26.5

require (
	github.com/mark3labs/mcp-go v0.57.0 // Shutdown closes active sessions (upstream PR 926)
	github.com/pelletier/go-toml/v2 v2.3.1 // fuzz-found datetime-unmarshal panic fix
	github.com/spf13/cobra v1.10.2 // keep Cobra coordinated with pflag 1.0.10
	github.com/spf13/pflag v1.0.10 // coordinated Cobra/pflag bump
	modernc.org/sqlite v1.52.0 // embeds SQLite 3.53.2; bump with modernc.org/libc in lockstep
)

require (
	github.com/gofrs/flock v0.13.1
	golang.org/x/text v0.41.0
)

require (
	github.com/dlclark/regexp2 v1.11.5 // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/google/jsonschema-go v0.4.2 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	github.com/santhosh-tekuri/jsonschema/v6 v6.0.2 // indirect
	github.com/spf13/cast v1.7.1 // indirect
	github.com/yosida95/uritemplate/v3 v3.0.2 // indirect
	golang.org/x/sys v0.47.0 // indirect
	modernc.org/libc v1.72.3 // indirect; MUST equal the version modernc.org/sqlite's go.mod pins (gitlab.com/cznic/sqlite#177) — gated by `make lockstep`
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.11.0 // indirect
)
