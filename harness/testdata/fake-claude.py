#!/usr/bin/env python3
# A fake claude for the tests of the driver of Claude Code. It speaks the
# stream-json lines of claude -p, asks the PreToolUse hook that it finds in
# --settings about one Write call, exits with tool_deferred when the hook
# says defer, and continues the deferred call when it runs with --resume.
import json
import os
import sys
import urllib.request

args = sys.argv[1:]


def arg(name):
    if name in args:
        return args[args.index(name) + 1]
    return None


home = os.environ["CLAUDE_CONFIG_DIR"]
sid = arg("--session-id") or arg("--resume")
resume = "--resume" in args
state_path = os.path.join(home, "fake-" + sid + ".json")
with open(os.path.join(home, "fake-calls.jsonl"), "a") as f:
    env = {k: os.environ.get(k, "") for k in ("HOME", "CLAUDE_CONFIG_DIR", "ANTHROPIC_API_KEY", "ANTHROPIC_BASE_URL", "FW_RUN_DIR", "FW_TEST_RUN_INFO")}
    f.write(json.dumps({"args": args, "env": env, "cwd": os.getcwd()}) + "\n")

hook = json.loads(arg("--settings"))["hooks"]["PreToolUse"][0]["hooks"][0]["url"]
opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
usage = {"input_tokens": 50, "output_tokens": 10, "cache_read_input_tokens": 5, "cache_creation_input_tokens": 0}


def out(o):
    o.setdefault("session_id", sid)
    print(json.dumps(o), flush=True)


def ask(tid, name, inp):
    body = json.dumps({"session_id": sid, "hook_event_name": "PreToolUse", "tool_name": name,
                       "tool_input": inp, "tool_use_id": tid}).encode()
    req = urllib.request.Request(hook, data=body, headers={"Content-Type": "application/json"})
    resp = json.load(opener.open(req))["hookSpecificOutput"]
    return resp["permissionDecision"], resp.get("permissionDecisionReason", "")


def result(text, **extra):
    o = {"type": "result", "subtype": "success", "is_error": False, "num_turns": 2, "result": text,
         "stop_reason": "end_turn", "total_cost_usd": 0.01, "usage": usage, "permission_denials": []}
    o.update(extra)
    out(o)


def run_tool(tid, name, inp):
    decision, reason = ask(tid, name, inp)
    with open(os.path.join(home, "fake-decisions.jsonl"), "a") as f:
        f.write(json.dumps({"id": tid, "decision": decision, "reason": reason}) + "\n")
    if decision == "defer":
        with open(state_path, "w") as f:
            json.dump({"deferred": {"id": tid, "name": name, "input": inp}}, f)
        result("", stop_reason="tool_deferred", deferred_tool_use={"id": tid, "name": name, "input": inp})
        sys.exit(0)
    if decision == "allow":
        with open(inp["file_path"], "w") as f:
            f.write(inp["content"])
        content, err = "File created", False
    else:
        content, err = reason, True
    out({"type": "user", "parent_tool_use_id": None,
         "message": {"role": "user", "content": [{"type": "tool_result", "tool_use_id": tid, "content": content, "is_error": err}]}})
    out({"type": "assistant", "parent_tool_use_id": None,
         "message": {"id": "msg_2", "model": arg("--model"), "role": "assistant", "stop_reason": "end_turn",
                     "content": [{"type": "text", "text": "Done."}], "usage": usage}})
    result("Done.")


out({"type": "system", "subtype": "init", "model": arg("--model"), "claude_code_version": "9.9.9",
     "permissionMode": "default", "tools": ["Bash", "Write"], "cwd": os.getcwd()})
state = {}
if os.path.exists(state_path):
    with open(state_path) as f:
        state = json.load(f)
if resume and state.get("deferred"):
    d = state.pop("deferred")
    with open(state_path, "w") as f:
        json.dump(state, f)
    run_tool(d["id"], d["name"], d["input"])
for line in sys.stdin:
    msg = json.loads(line)
    text = msg["message"]["content"]
    if "read twice" in text:
        # Two tool calls, one in each of two responses.
        for i, tid in enumerate(("toolu_1", "toolu_2")):
            inp = {"file_path": "f%d.txt" % i}
            out({"type": "assistant", "parent_tool_use_id": None,
                 "message": {"id": "msg_%d" % i, "model": arg("--model"), "role": "assistant", "stop_reason": "tool_use",
                             "content": [{"type": "tool_use", "id": tid, "name": "Read", "input": inp}], "usage": usage}})
            decision, reason = ask(tid, "Read", inp)
            with open(os.path.join(home, "fake-decisions.jsonl"), "a") as f:
                f.write(json.dumps({"id": tid, "decision": decision, "reason": reason}) + "\n")
            out({"type": "user", "parent_tool_use_id": None,
                 "message": {"role": "user", "content": [{"type": "tool_result", "tool_use_id": tid, "content": reason or "contents", "is_error": decision != "allow"}]}})
        out({"type": "assistant", "parent_tool_use_id": None,
             "message": {"id": "msg_end", "model": arg("--model"), "role": "assistant", "stop_reason": "end_turn",
                         "content": [{"type": "text", "text": "Read both."}], "usage": usage}})
        result("Read both.")
    elif "two reads" in text:
        # Two tool calls in one response, as Claude makes them at once.
        calls = [("toolu_a", {"file_path": "a.txt"}), ("toolu_b", {"file_path": "b.txt"})]
        for i, (tid, inp) in enumerate(calls):
            out({"type": "assistant", "parent_tool_use_id": None,
                 "message": {"id": "msg_r", "model": arg("--model"), "role": "assistant",
                             "stop_reason": "tool_use" if i == len(calls) - 1 else None,
                             "content": [{"type": "tool_use", "id": tid, "name": "Read", "input": inp}], "usage": usage}})
        results = []
        for tid, inp in calls:
            decision, reason = ask(tid, "Read", inp)
            with open(os.path.join(home, "fake-decisions.jsonl"), "a") as f:
                f.write(json.dumps({"id": tid, "decision": decision, "reason": reason}) + "\n")
            results.append({"type": "tool_result", "tool_use_id": tid, "content": reason or "contents", "is_error": decision != "allow"})
        out({"type": "user", "parent_tool_use_id": None, "message": {"role": "user", "content": results}})
        out({"type": "assistant", "parent_tool_use_id": None,
             "message": {"id": "msg_r2", "model": arg("--model"), "role": "assistant", "stop_reason": "end_turn",
                         "content": [{"type": "text", "text": "Read both."}], "usage": usage}})
        result("Read both.")
    elif "hello" in text:
        inp = {"file_path": "hello.txt", "content": "hello"}
        out({"type": "assistant", "parent_tool_use_id": None,
             "message": {"id": "msg_1", "model": arg("--model"), "role": "assistant", "stop_reason": None,
                         "content": [{"type": "text", "text": "I will write the file."}], "usage": usage}})
        out({"type": "assistant", "parent_tool_use_id": None,
             "message": {"id": "msg_1", "model": arg("--model"), "role": "assistant", "stop_reason": "tool_use",
                         "content": [{"type": "tool_use", "id": "toolu_1", "name": "Write", "input": inp}], "usage": usage}})
        run_tool("toolu_1", "Write", inp)
    else:
        out({"type": "assistant", "parent_tool_use_id": None,
             "message": {"id": "msg_x", "model": arg("--model"), "role": "assistant", "stop_reason": "end_turn",
                         "content": [{"type": "text", "text": "Again."}], "usage": usage}})
        result("Again.")
