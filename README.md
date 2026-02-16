# lcm-tui

A terminal UI for browsing OpenClaw LCM (Lossless Context Management) data.

Browse agents → sessions → conversations → LCM summaries interactively.

## Install

```bash
go install github.com/Martian-Engineering/lcm-tui@latest
```

Or build from source:

```bash
git clone git@github.com:Martian-Engineering/lcm-tui.git
cd lcm-tui
go build -o lcm-tui .
```

## Usage

```bash
./lcm-tui
```

Navigate with arrow keys, Enter to drill in, `b` to go back, `q` to quit.

## Requirements

- OpenClaw with LCM enabled (`~/.openclaw/lcm.db` and `~/.openclaw/agents/` must exist)
