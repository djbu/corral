#!/usr/bin/env python3
"""Split spike/hooks/out/*.jsonl capture lines into one fixture file per
(event, context) into testdata/hooks/2.1.224/. Picks the most illustrative
sample per bucket (first occurrence, skipping empty stdin). Throwaway spike
script, not part of corral proper.
"""
import json
import os
import glob

OUT_DIR = "/Users/danielbecerra/code/research/corral/spike/hooks/out"
DEST = "/Users/danielbecerra/code/research/corral/testdata/hooks/2.1.224"

# (source_glob, hook_event_name, extra_predicate, dest_filename)
def load_all():
    records = []
    for f in sorted(glob.glob(OUT_DIR + "/*.jsonl")):
        run = os.path.basename(f).replace(".jsonl", "")
        for line in open(f):
            line = line.strip()
            if not line:
                continue
            try:
                r = json.loads(line)
            except Exception:
                continue
            sj = r.get("stdin_json")
            if not sj:
                continue
            r["_run"] = run
            records.append(r)
    return records

def write(name, sj):
    path = os.path.join(DEST, name)
    with open(path, "w") as f:
        json.dump(sj, f, indent=2)
        f.write("\n")
    print("wrote", path)

def main():
    recs = load_all()
    os.makedirs(DEST, exist_ok=True)

    def first(pred):
        for r in recs:
            sj = r["stdin_json"]
            if pred(r, sj):
                return sj
        return None

    buckets = [
        ("sessionstart-startup.json", lambda r, sj: sj.get("hook_event_name") == "SessionStart" and sj.get("source") == "startup"),
        ("sessionstart-resume.json", lambda r, sj: sj.get("hook_event_name") == "SessionStart" and sj.get("source") == "resume"),
        ("userpromptsubmit.json", lambda r, sj: sj.get("hook_event_name") == "UserPromptSubmit"),
        ("pretooluse-bash.json", lambda r, sj: sj.get("hook_event_name") == "PreToolUse" and sj.get("tool_name") == "Bash"),
        ("pretooluse-agent-task.json", lambda r, sj: sj.get("hook_event_name") == "PreToolUse" and sj.get("tool_name") == "Agent"),
        ("posttooluse-bash.json", lambda r, sj: sj.get("hook_event_name") == "PostToolUse" and sj.get("tool_name") == "Bash"),
        ("posttooluse-agent-task.json", lambda r, sj: sj.get("hook_event_name") == "PostToolUse" and sj.get("tool_name") == "Agent"),
        ("permissionrequest-bash.json", lambda r, sj: sj.get("hook_event_name") == "PermissionRequest"),
        ("notification-permission.json", lambda r, sj: sj.get("hook_event_name") == "Notification" and sj.get("notification_type") == "permission_prompt"),
        ("stop-normal.json", lambda r, sj: sj.get("hook_event_name") == "Stop" and sj.get("stop_hook_active") is False),
        ("stop-hook-active-true.json", lambda r, sj: sj.get("hook_event_name") == "Stop" and sj.get("stop_hook_active") is True),
        ("subagentstart.json", lambda r, sj: sj.get("hook_event_name") == "SubagentStart"),
        ("subagentstop.json", lambda r, sj: sj.get("hook_event_name") == "SubagentStop" and sj.get("agent_type") == "general-purpose"),
        ("subagentstop-empty-agenttype.json", lambda r, sj: sj.get("hook_event_name") == "SubagentStop" and sj.get("agent_type") == ""),
        ("sessionend-other.json", lambda r, sj: sj.get("hook_event_name") == "SessionEnd" and sj.get("reason") == "other"),
    ]
    for name, pred in buckets:
        sj = first(pred)
        if sj is not None:
            write(name, sj)
        else:
            print("MISSING (no sample matched):", name)

    # dump distinct hook_event_name set observed overall, for the NOTES
    names = sorted({r["stdin_json"].get("hook_event_name") for r in recs if r["stdin_json"].get("hook_event_name")})
    print("\nDistinct hook_event_name values observed across all runs:", names)

if __name__ == "__main__":
    main()
