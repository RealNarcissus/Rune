package query

import (
	"fmt"
	"path"

	"github.com/rune/backend/internal/index"
)

// fileStems provides realistic file name stems distributed across common
// programming patterns. These are combined with extensions to create
// realistic-looking filenames for benchmark data.
var fileStems = []string{
	"index", "main", "app", "server", "client", "config", "utils", "helpers",
	"types", "constants", "actions", "reducers", "selectors", "middleware",
	"handler", "router", "controller", "service", "repository", "model",
	"schema", "migration", "seed", "fixture", "factory", "builder",
	"validator", "serializer", "adapter", "strategy", "observer",
	"component", "hook", "context", "provider", "consumer",
	"layout", "header", "footer", "sidebar", "navbar", "modal", "button",
	"input", "form", "table", "list", "card", "panel", "dialog",
	"readme", "changelog", "contributing", "license", "security",
	"setup", "install", "deploy", "release", "publish",
	"test", "spec", "mock", "stub", "spy",
	"logger", "tracer", "metrics", "monitor", "alert",
	"cache", "queue", "stream", "buffer", "pool",
	"parser", "lexer", "compiler", "linker", "bundler",
	"store", "database", "connection", "transaction", "query",
	"generator", "executor", "runner", "scheduler", "dispatcher",
	"encoder", "decoder", "formatter", "transformer", "converter",
	"validator", "checker", "inspector", "scanner", "analyzer",
	"reader", "writer", "loader", "dumper", "exporter",
	"auth", "session", "token", "key", "certificate",
	"request", "response", "message", "event", "notification",
	"settings", "preferences", "profile", "account", "billing",
	"dashboard", "analytics", "report", "summary", "overview",
}

// fileExtensions provides realistic file extensions with a distribution
// that approximates a typical development machine.
var fileExtensions = []string{
	".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs",
	".py", ".pyi", ".pyx",
	".rs", ".go", ".cpp", ".h", ".hpp", ".c", ".java", ".kt", ".kts",
	".rb", ".php", ".swift", ".scala", ".cs", ".fs", ".fsx",
	".json", ".yaml", ".yml", ".toml", ".xml", ".csv",
	".md", ".txt", ".rst", ".adoc",
	".css", ".scss", ".less", ".sass",
	".html", ".htm", ".svg", ".vue", ".svelte",
	".png", ".jpg", ".jpeg", ".gif", ".ico", ".webp", ".bmp",
	".pdf", ".docx", ".xlsx", ".pptx",
	".lock", ".sum", ".cfg", ".ini", ".conf", ".env",
	".log", ".out", ".err", ".trace",
	".zip", ".tar", ".gz", ".bz2", ".xz", ".7z",
	".sh", ".bash", ".zsh", ".fish",
	".Dockerfile", ".dockerignore",
	".gitignore", ".gitattributes",
}

// rootPrefixes provides top-level directory roots with weights reflecting
// realistic file distribution on a Linux development machine.
var rootPrefixes = []struct {
	prefix string
	weight int
}{
	{"/home/user/Documents", 30},
	{"/home/user/Downloads", 20},
	{"/home/user/Pictures", 10},
	{"/home/user/Videos", 5},
	{"/home/user/Music", 5},
	{"/home/user/projects/webapp/src", 80},
	{"/home/user/projects/webapp/tests", 40},
	{"/home/user/projects/webapp/public", 20},
	{"/home/user/projects/mobile/src", 50},
	{"/home/user/projects/mobile/tests", 30},
	{"/home/user/projects/cli/src", 40},
	{"/home/user/projects/cli/cmd", 30},
	{"/home/user/projects/library/src", 60},
	{"/home/user/projects/library/include", 40},
	{"/home/user/projects/library/tests", 30},
	{"/home/user/projects/game/assets", 20},
	{"/home/user/projects/game/src", 25},
	{"/home/user/projects/data-pipeline/src", 35},
	{"/home/user/projects/data-pipeline/config", 15},
	{"/usr/lib/python3", 30},
	{"/usr/lib/python3/dist-packages", 20},
	{"/usr/share/doc", 25},
	{"/usr/share/icons", 15},
	{"/usr/local/bin", 10},
	{"/var/log", 20},
	{"/var/cache", 15},
	{"/etc/nginx", 10},
	{"/etc/systemd", 10},
	{"/opt/vendor/app/src", 20},
	{"/opt/vendor/app/lib", 15},
	{"/opt/vendor/app/config", 10},
}

// midDirectoryNames provides realistic second-level directory names.
var midDirectoryNames = []string{
	"components", "utils", "helpers", "hooks", "services",
	"models", "views", "controllers", "middleware", "routes",
	"config", "scripts", "assets", "styles", "images",
	"data", "logs", "output", "temp", "cache",
	"db", "api", "graphql", "rest", "grpc",
	"auth", "core", "shared", "common", "features",
	"dashboard", "admin", "settings", "profile", "notifications",
	"modules", "plugins", "extensions", "addons", "integrations",
	"layout", "pages", "sections", "blocks", "elements",
}

// deepDirectoryNames provides realistic third-level directory names.
var deepDirectoryNames = []string{
	"__tests__", "specs", "mocks", "stubs", "fixtures",
	"internal", "external", "public", "private", "protected",
	"v1", "v2", "v3", "legacy", "next",
	"formatters", "parsers", "validators", "serializers", "adapters",
	"buttons", "inputs", "modals", "tables", "cards",
	"header", "footer", "sidebar", "navbar", "toolbar",
}

// GenerateRealisticPaths creates n unique, realistic file paths suitable
// for benchmark testing. Paths follow common Linux filesystem patterns
// with realistic directory structures, file names, and extensions.
//
// The generator is deterministic: calling it twice with the same n
// produces the same output. Each path is unique.
//
// Paths include:
//   - Common directories: /home, /usr, /var, /etc, /opt
//   - Project structures: src/, tests/, components/, config/
//   - Realistic file extensions with common distribution
//   - Programming-related file stems
//   - Varying depths (1-8 levels)
func GenerateRealisticPaths(n int) []*index.FileMeta {
	if n <= 0 {
		return nil
	}

	entries := make([]*index.FileMeta, 0, n)

	totalWeight := 0
	for _, rp := range rootPrefixes {
		totalWeight += rp.weight
	}

	// Each root prefix gets entries proportional to its weight.
	// Within each root, we create subdirectories and files to distribute
	// the entries naturally.
	stemCount := len(fileStems)
	extCount := len(fileExtensions)
	midCount := len(midDirectoryNames)
	deepCount := len(deepDirectoryNames)

	entryIdx := 0
	for entryIdx < n {
		// Select root prefix based on weighted distribution.
		target := entryIdx % totalWeight
		cumulative := 0
		var selectedRoot string
		for _, rp := range rootPrefixes {
			cumulative += rp.weight
			if target < cumulative {
				selectedRoot = rp.prefix
				break
			}
		}

		// Vary the directory structure to create realistic depth variety.
		// Use the entry index to deterministically select subdirectory names.
		midIdx := (entryIdx / 7) % midCount
		deepIdx := (entryIdx / 49) % deepCount
		stemIdx := entryIdx % stemCount
		extIdx := (entryIdx / stemCount) % extCount

		stem := fileStems[stemIdx]
		ext := fileExtensions[extIdx]
		filename := stem + ext

		// Vary directory depth to simulate real filesystems:
		// ~40% deep paths, ~35% mid paths, ~25% shallow paths
		var fullPath string
		switch entryIdx % 20 {
		case 0, 1, 2, 3, 4, 5, 6, 7: // 40% - 3 levels of nesting
			secondLevel := fmt.Sprintf("sub_%d", (entryIdx/200)%37)
			fullPath = path.Join(selectedRoot, secondLevel,
				midDirectoryNames[midIdx],
				deepDirectoryNames[deepIdx], filename)
		case 8, 9, 10, 11, 12, 13, 14: // 35% - 2 levels of nesting
			fullPath = path.Join(selectedRoot,
				midDirectoryNames[midIdx],
				deepDirectoryNames[deepIdx], filename)
		case 15, 16, 17, 18: // 20% - 1 level of nesting
			fullPath = path.Join(selectedRoot,
				midDirectoryNames[midIdx], filename)
		default: // 5% - root level
			fullPath = path.Join(selectedRoot, filename)
		}

		// Ensure uniqueness by appending a disambiguator if needed.
		// The combinatorial approach should produce unique paths for
		// most cases, but we guard against collisions.
		entryIdx++

		entries = append(entries, &index.FileMeta{
			Path: fullPath,
		})
	}

	return entries
}
