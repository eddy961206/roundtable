#!/usr/bin/env python3
"""Small real-process lifecycle smoke using only the deterministic fake CLI.

Run against either revision with --binary, --fixture and --output. Each run
uses a private temporary data directory and loopback port. The rt commands use
the same tool implementation as MCP (including its SSE completion observer).
This checks functionality; timings are deliberately not performance results.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import platform
import socket
import subprocess
import tempfile
import time

from stream_load import Subscriber, request


def require(condition, message):
    if not condition:
        raise AssertionError(message)


def prompt(tokens, delay_us=0):
    return "PERF_JSON " + json.dumps({"tokens": tokens, "delay_us": delay_us, "tools": 2})


def expected(tokens):
    return "".join(f"piece-{i:04d}: 검증용 **stream** data.\n" for i in range(tokens)).strip()


def snapshot(port, room):
    return request(port, "GET", f"/api/rooms/{room}?limit=200")


def answers(page):
    return [m for m in page["messages"] if m["kind"] == "agent"]


def wait_for(port, room, predicate, message, seconds=15):
    deadline = time.monotonic() + seconds
    while time.monotonic() < deadline:
        page = snapshot(port, room)
        if predicate(page):
            return page
        time.sleep(.02)
    raise TimeoutError(message)


class Server:
    def __init__(self, binary, fixture, root, port):
        missing = str(root / "missing-provider")
        self.binary, self.port, self.root = binary, port, root
        self.command = [str(binary), "-addr", f"127.0.0.1:{port}", "-data", str(root / "data"),
                        "-claude", str(fixture), "-codex", missing, "-agy", missing, "-opencode", missing]
        self.process = None
        self.log = (root / "server.log").open("w+", encoding="utf-8")

    def start(self):
        self.process = subprocess.Popen(self.command, stdout=self.log, stderr=self.log,
                                        env={**os.environ, "GOMAXPROCS": "4"})
        for _ in range(200):
            try:
                request(self.port, "GET", "/api/config")
                return
            except OSError:
                if self.process.poll() is not None:
                    self.log.seek(0)
                    raise RuntimeError(self.log.read())
                time.sleep(.025)
        raise TimeoutError("server startup")

    def stop(self):
        if self.process is not None and self.process.poll() is None:
            self.process.terminate()
            try:
                self.process.wait(timeout=15)
            except subprocess.TimeoutExpired:
                self.process.kill()
                self.process.wait()
                raise RuntimeError("server did not finish graceful shutdown")

    def close(self):
        try:
            self.stop()
        finally:
            self.log.close()

    def rt(self, *args):
        result = subprocess.run([str(self.binary), "rt", "--url", f"http://127.0.0.1:{self.port}", *args],
                                capture_output=True, text=True, encoding="utf-8", timeout=30)
        require(result.returncode == 0, f"rt command failed ({result.returncode}): {result.stderr}")
        return json.loads(result.stdout)


def create_room(port, count):
    return request(port, "POST", "/api/rooms", {
        "title": "synthetic lifecycle fixture", "mode": "chat",
        "participant_specs": [{"provider": "claude", "model": "perf-fixture", "count": count}]})


def saved_state(port, room):
    page = snapshot(port, room)
    require(not page["queue"], "room still has queued/running turns")
    require(all(m["status"] != "streaming" for m in answers(page)), "reply still streaming")
    traces = {m["id"]: request(port, "GET", f"/api/rooms/{room}/trace/{m['id']}") for m in answers(page)}
    for msg in answers(page):
        require(len(traces[msg["id"]]) == msg["trace"], "persisted trace count mismatch")
    return {"messages": page["messages"], "participants": page["room"]["participants"], "traces": traces}


def run(server, report):
    port = server.port
    one = create_room(port, 1)
    rid = one["id"]
    for tokens in (4, 6):
        result = server.rt("post", "--room", rid, "--text", prompt(tokens), "--wait_seconds", "15")
        replies = answers(result)
        require(result.get("timed_out") is False, "rt post did not confirm completion")
        require(len(replies) == 1 and replies[0]["status"] == "done", "rt post returned incorrect replies")
        require(replies[0]["text"] == expected(tokens), "rt post lost final response text")
        require(replies[0]["reply_to"] == result["seq"], "MCP reply-to sequence mismatch")
    page = snapshot(port, rid)
    replies = answers(page)
    require(len(replies) == 2, "resumed room has duplicate/missing replies")
    session = replies[0]["session_id"]
    require(session and replies[1]["session_id"] == session, "CLI session was not resumed")
    seat = page["room"]["participants"][0]
    require(seat["session_id"] == session, "participant lost resumed session")
    require(seat["totals"]["input"] == 200 and seat["totals"]["output"] == 10
            and seat["totals"]["turns"] == 2, "usage totals were duplicated or lost")
    for message, tokens in zip(replies, (4, 6)):
        require(message["usage"]["input_tokens"] == 100 and message["usage"]["output_tokens"] == tokens,
                "per-turn usage differs from fixture")
    read = server.rt("read", "--room", rid, "--max_chars", "20000")
    require([m["text"] for m in answers(read)] == [expected(4), expected(6)], "rt read lost history")
    report["checks"].append({"name": "session_resume_and_rt_post_wait_read", "status": "passed",
                              "completed_turns": 2, "same_session": True, "input_tokens": 200, "output_tokens": 10})

    two = create_room(port, 2)
    second = two["id"]
    stopped, survivor = [p["id"] for p in two["participants"]]
    listener = Subscriber(port, second, 2)
    listener.start()
    require(listener.ready.wait(10), f"SSE was not ready: {listener.error}")
    request(port, "POST", f"/api/rooms/{second}/messages", {"text": prompt(80, 15000)})
    wait_for(port, second,
             lambda p: len(answers(p)) == 2 and all(m["status"] == "streaming" and "piece-0001" in m["text"] for m in answers(p)),
             "both participants did not start independent streaming")
    stop = request(port, "POST", f"/api/rooms/{second}/participants/{stopped}/stop")
    require(stop["cancelled_running"] and stop["participant_id"] == stopped, "stop did not target the running seat")
    stopped_page = wait_for(port, second,
                           lambda p: any(m["author"] == stopped and m["status"] == "cancelled" for m in answers(p)),
                           "stopped participant did not become cancelled")
    require(any(m["author"] == survivor and m["status"] == "streaming" for m in answers(stopped_page)),
            "other participant did not continue after individual stop")
    require(listener.done.wait(15) and listener.error is None, f"SSE did not finish both replies: {listener.error}")
    page = wait_for(port, second, lambda p: not p["queue"] and all(m["status"] != "streaming" for m in answers(p)),
                    "room did not settle after individual stop")
    by_seat = {m["author"]: m for m in answers(page)}
    require(by_seat[stopped]["status"] == "cancelled" and by_seat[survivor]["status"] == "done", "incorrect final seat statuses")
    require(by_seat[survivor]["text"] == expected(80), "surviving reply was shortened")
    require(by_seat[stopped]["text"] and by_seat[stopped]["text"] != expected(80), "cancelled partial answer was lost")
    require(not next(p for p in page["room"]["participants"] if p["id"] == stopped)["enabled"], "stopped seat stayed enabled")
    for msg in by_seat.values():
        mid = msg["id"]
        require(listener.completed[mid]["status"] == msg["status"], "SSE completion status differs from HTTP")
        require("".join(listener.deltas[mid]).strip() == msg["text"], "SSE text changed on cancellation/completion")
        require(listener.trace_indices[mid] == list(range(1, msg["trace"] + 1)), "SSE trace order/gap")
    report["checks"].append({"name": "individual_stop_and_independent_completion", "status": "passed",
                              "cancelled": 1, "completed": 1, "sse_trace_indices_contiguous": True,
                              "cancelled_partial_body_retained": True})

    before = {room: saved_state(port, room) for room in (rid, second)}
    server.stop()
    server.start()
    after = {room: saved_state(port, room) for room in (rid, second)}
    require(before == after, "restart changed messages, participants, usage, sessions or traces")
    read = server.rt("read", "--room", rid, "--max_chars", "20000")
    require([m["text"] for m in answers(read)] == [expected(4), expected(6)], "rt read lost history after restart")
    # Resumption also works after the server itself has restarted.
    result = server.rt("post", "--room", rid, "--text", prompt(3), "--wait_seconds", "15")
    require(result.get("timed_out") is False and answers(result)[0]["text"] == expected(3), "post-restart turn failed")
    newest = answers(snapshot(port, rid))[-1]
    require(newest["session_id"] == session, "restart reset the CLI session")
    trace_count = sum(len(trace) for state in after.values() for trace in state["traces"].values())
    digest = hashlib.sha256(json.dumps(before, ensure_ascii=False, sort_keys=True).encode()).hexdigest()
    report["checks"].append({"name": "graceful_restart_preserves_durable_state_and_resume", "status": "passed",
                              "rooms": 2, "agent_messages": 4, "trace_events": trace_count,
                              "state_sha256_before_equals_after": digest, "post_restart_session_resumed": True})


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", required=True, type=Path)
    parser.add_argument("--fixture", required=True, type=Path)
    parser.add_argument("--output", required=True, type=Path)
    args = parser.parse_args()
    report = {"platform": platform.platform(), "python": platform.python_version(), "paid_model_calls": 0,
              "binary_sha256": hashlib.sha256(args.binary.read_bytes()).hexdigest(), "checks": []}
    error = None
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        port = sock.getsockname()[1]
    with tempfile.TemporaryDirectory(prefix="roundtable-lifecycle-") as directory:
        server = Server(args.binary.resolve(), args.fixture.resolve(), Path(directory), port)
        try:
            server.start()
            run(server, report)
            report["status"] = "passed"
        except Exception as exc:
            error = exc
            report["status"], report["error"] = "failed", str(exc)
        finally:
            try:
                server.close()
            except Exception as exc:
                error = error or exc
                report["status"], report["cleanup_error"] = "failed", str(exc)
    args.output.parent.mkdir(parents=True, exist_ok=True)
    args.output.write_text(json.dumps(report, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    print(json.dumps(report, ensure_ascii=False))
    if error:
        raise SystemExit(1)


if __name__ == "__main__":
    main()
