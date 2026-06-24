module zotero-pp-cli

go 1.26.3

require (
	github.com/pelletier/go-toml/v2 v2.3.1
	github.com/spf13/cobra v1.10.2 // PATCH(glean zotero-pp-cli-82c3d04f2a24ce53): keep Cobra coordinated with pflag 1.0.10.
)

require modernc.org/sqlite v1.52.0

require (
	github.com/mark3labs/mcp-go v0.55.0 // PATCH(glean zotero-pp-cli-9ed6684bc0654c41): pick up transport/session hardening after 0.52.0.
	github.com/spf13/pflag v1.0.10 // PATCH(glean zotero-pp-cli-82c3d04f2a24ce53): coordinated Cobra/pflag bump.
)

require (
	github.com/dlclark/regexp2 v1.11.5 // indirect; PATCH(glean zotero-pp-cli-100f6829338591c9): force patched transitive version.
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
	golang.org/x/text v0.38.0 // indirect; PATCH(glean zotero-pp-cli-261ed9d68c4d6488): pin current x/text over stale indirect floor.
	golang.org/x/tools v0.46.0 // indirect
	modernc.org/libc v1.72.3 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.11.0 // indirect
)
