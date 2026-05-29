# Rune v1 - Development Guide

Rune is a premium keyboard-first desktop search utility built with Svelte, TailwindCSS, Tauri v2, and a Go sidecar backend indexer.

## Commands

### Frontend & Tauri Desktop Shell
```bash
pnpm install          # Install frontend and Tauri dependencies
pnpm tauri dev        # Run the Tauri desktop app in development mode
pnpm tauri build      # Build the production Tauri release bundle
```

### Go Sidecar Backend (`backend`)
```bash
cd backend && go run ./cmd/runed   # Run the backend indexer daemon
cd backend && go test ./...          # Run backend unit tests
cd backend && go test -v ./internal/query -run=Benchmark  # Run search benchmarks
```

---

## Agent skills

### Issue tracker

Local markdown files in the repository under `.scratch/`. See [issue-tracker.md](file:///home/charleton/Desktop/agentProjects/Droid/Rune/docs/agents/issue-tracker.md).

### Triage labels

State-machine label mapping matching canonical roles. See [triage-labels.md](file:///home/charleton/Desktop/agentProjects/Droid/Rune/docs/agents/triage-labels.md).

### Domain docs

Single-context documentation layout using `CONTEXT.md` and `docs/adr/`. See [domain.md](file:///home/charleton/Desktop/agentProjects/Droid/Rune/docs/agents/domain.md).

---

## Skill routing

When the user's request matches an available skill, invoke it via the Skill tool. When in doubt, invoke the skill.

Key routing rules:
- Product ideas/brainstorming → invoke /office-hours
- Strategy/scope → invoke /plan-ceo-review
- Architecture → invoke /plan-eng-review
- Design system/plan review → invoke /design-consultation or /plan-design-review
- Full review pipeline → invoke /autoplan
- Bugs/errors → invoke /investigate
- QA/testing site behavior → invoke /qa or /qa-only
- Code review/diff check → invoke /review
- Visual polish → invoke /design-review
- Ship/deploy/PR → invoke /ship or /land-and-deploy
- Save progress → invoke /context-save
- Resume context → invoke /context-restore
