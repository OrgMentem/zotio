module zotio

go 1.26

require (
	github.com/mark3labs/mcp-go v0.55.0 // transport/session hardening after 0.52.0
	github.com/pelletier/go-toml/v2 v2.3.1 // fuzz-found datetime-unmarshal panic fix
	github.com/spf13/cobra v1.10.2 // keep Cobra coordinated with pflag 1.0.10
	github.com/spf13/pflag v1.0.10 // coordinated Cobra/pflag bump
	modernc.org/sqlite v1.52.0 // CVE-2025-3277 concat_ws heap overflow fix
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
	golang.org/x/sys v0.46.0 // indirect
	golang.org/x/text v0.38.0 // indirect
	golang.org/x/tools v0.46.0 // indirect
	modernc.org/libc v1.72.3 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.11.0 // indirect
)
