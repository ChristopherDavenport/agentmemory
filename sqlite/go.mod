module github.com/ChristopherDavenport/agentmemory/sqlite

go 1.26.0

require (
	github.com/ChristopherDavenport/agentmemory v0.0.13
	modernc.org/sqlite v1.62.0
)

require (
	github.com/ChristopherDavenport/agenttool v0.0.28 // indirect
	github.com/ChristopherDavenport/openresponses v0.0.21 // indirect
	github.com/dustin/go-humanize v1.1.0 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/ncruces/go-strftime v1.1.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	golang.org/x/sys v0.49.0 // indirect
	modernc.org/libc v1.80.1 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.12.1 // indirect
)

// The require names the released root a consumer fetches; the replace
// builds against the tree.
replace github.com/ChristopherDavenport/agentmemory => ../
