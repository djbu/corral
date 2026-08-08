#!/usr/bin/env python3
"""Generate --settings JSON files for the M2 Step-0 hook spike.
Throwaway, spike/hooks/ only. Not part of corral proper.
"""
import json
import sys

CAP_BIN = "/Users/danielbecerra/code/research/corral/spike/hooks/capture/corral-hookcap"

TOOL_EVENTS = ["PreToolUse", "PostToolUse", "PermissionRequest", "PostToolUseFailure"]
NONTOOL_EVENTS = [
    "SessionStart", "UserPromptSubmit", "Notification", "Stop",
    "SubagentStart", "SubagentStop", "PreCompact", "SessionEnd", "StopFailure",
    "TeammateIdle",
]

def cmd(out_file, timeout=5):
    c = {"type": "command", "command": f"{CAP_BIN} --out {out_file}", "timeout": timeout}
    return c

def build(out_file, matcher_style="star", timeout=5):
    """matcher_style: 'star' -> "*" matcher on tool events, 'omit' -> no matcher key at all
    (tests V20 for tool events too), 'empty' -> matcher: "" """
    hooks = {}
    for ev in TOOL_EVENTS:
        entry = {"hooks": [cmd(out_file, timeout)]}
        if matcher_style == "star":
            entry["matcher"] = "*"
        elif matcher_style == "empty":
            entry["matcher"] = ""
        # 'omit' -> no matcher key
        hooks[ev] = [entry]
    for ev in NONTOOL_EVENTS:
        hooks[ev] = [{"hooks": [cmd(out_file, timeout)]}]
    return {"hooks": hooks}

if __name__ == "__main__":
    out_file = sys.argv[1]
    matcher_style = sys.argv[2] if len(sys.argv) > 2 else "star"
    dest = sys.argv[3] if len(sys.argv) > 3 else "/dev/stdout"
    settings = build(out_file, matcher_style)
    with open(dest, "w") as f:
        json.dump(settings, f, indent=2)
    print(f"wrote {dest} (capture->{out_file}, matcher_style={matcher_style})", file=sys.stderr)
