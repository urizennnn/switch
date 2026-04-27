# Switch

> **⚠️ Early development — not production ready.**

Switch gives you an **omni mind** — a unified AI consciousness made up of multiple concurrent agents that deliberate together on your problems. Enable as many AI clients as you want; they all act as one coordinated intelligence.

---

## Concept

Most AI tools give you one model answering one question. Switch distributes your problem across a fleet of minds that reason in parallel, challenge each other, and converge on a better answer than any single agent could produce alone.

You get one interface. The minds handle the rest.

---

## Features

- **Parallel deliberation** — multiple Claude agents reason simultaneously on your input
- **Coordinated output** — agents act as a single cohesive mind, not a noisy panel
- **Terminal-native UI** — keyboard-driven TUI built with Bubble Tea
- **Memory layer** — embedding store for retrieval across sessions *(in progress)*
- **Multi-provider** — Anthropic and OpenAI client wrappers *(in progress)*

---

## Keybindings

| Key | Action |
|-----|--------|
| `Ctrl+A` | Approve |
| `Ctrl+R` | Reject |
| `Ctrl+V` | Toggle minds view |
| `?` | Help |
| `Ctrl+C` | Quit |

---

## Stack

- **Go 1.26**
- [Bubble Tea](https://github.com/charmbracelet/bubbletea) — TUI framework
- [Lip Gloss](https://github.com/charmbracelet/lipgloss) — terminal styling
- Claude Sonnet 4.6 (default model)

---

## Getting Started

```bash
You can't 
---

## Status

Switch is under active development. Core orchestration, memory, and multi-provider support are scaffolded but not yet wired up. The TUI and agent coordination layer are functional.

Contributions and feedback welcome.
