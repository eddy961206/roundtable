#!/usr/bin/env python3
"""Exercise the real HTTP -> CLI -> JSONL -> SSE path without a paid model.

Only launches the supplied binaries, always with its own temporary data folder.
Linux process CPU/RSS/fd observations exclude the fixture and Python subscribers.
"""
import argparse
import collections
import http.client
import json
import os
from pathlib import Path
import platform
import socket
import statistics
import subprocess
import tempfile
import threading
import time


def request(port, method, path, body=None):
    conn = http.client.HTTPConnection("127.0.0.1", port, timeout=30)
    try:
        conn.request(method, path, None if body is None else json.dumps(body),
                     {"Content-Type": "application/json"})
        res = conn.getresponse()
        data = json.loads(res.read())
        if res.status >= 400:
            raise RuntimeError((res.status, data))
        return data
    finally:
        conn.close()


def process_sample(pid):
    root = Path(f"/proc/{pid}")
    if not root.exists():
        return {"cpu_ms": None, "rss_kib": None, "fds": None}
    stat = (root / "stat").read_text().split(")", 1)[1].split()
    status = dict(line.split(":", 1) for line in (root / "status").read_text().splitlines())
    return {"cpu_ms": (int(stat[11]) + int(stat[12])) * 1000 / os.sysconf("SC_CLK_TCK"),
            "rss_kib": int(status["VmRSS"].split()[0]), "fds": len(list((root / "fd").iterdir()))}


class Subscriber(threading.Thread):
    def __init__(self, port, room, participants):
        super().__init__(daemon=True)
        self.conn = http.client.HTTPConnection("127.0.0.1", port, timeout=30)
        self.path = f"/api/rooms/{room}/stream"
        self.participants = participants
        self.ready = threading.Event()
        self.done = threading.Event()
        self.error = None
        self.deltas = collections.defaultdict(list)
        self.trace_indices = collections.defaultdict(list)
        self.completed = {}
        self.complete_at = []
        self.events = 0

    def run(self):
        try:
            self.conn.request("GET", self.path)
            res = self.conn.getresponse()
            assert res.status == 200, res.status
            while True:
                line = res.readline()
                if not line:
                    raise RuntimeError("SSE disconnected before every reply completed")
                if line.startswith(b"event: hello"):
                    self.ready.set()
                if not line.startswith(b"data: {"):
                    continue
                event = json.loads(line[6:])
                self.events += 1
                kind, data = event["type"], event["data"]
                if kind == "delta":
                    self.deltas[data["id"]].append(data["text"])
                elif kind == "trace":
                    self.trace_indices[data["msg_id"]].append(data["index"])
                elif kind == "message" and data["kind"] == "agent" and data["status"] != "streaming":
                    self.completed[data["id"]] = data
                    self.complete_at.append(time.perf_counter())
                    if len(self.completed) == self.participants:
                        break
        except Exception as exc:
            self.error = str(exc)
        finally:
            self.conn.close()
            self.done.set()


def quantile(values, fraction):
    return sorted(values)[min(len(values) - 1, int((len(values) - 1) * fraction + .5))]


def run_sample(port, pid, participants, subscribers, tokens, delay_us):
    room = request(port, "POST", "/api/rooms", {
        "title": "synthetic performance fixture", "mode": "chat",
        "participant_specs": [{"provider": "claude", "model": "perf-fixture", "count": participants,
                               "role": "Keep every synthetic event intact. " * 32}]})
    rid = room["id"]
    listeners = [Subscriber(port, rid, participants) for _ in range(subscribers)]
    for listener in listeners:
        listener.start()
    for listener in listeners:
        if not listener.ready.wait(10):
            raise RuntimeError(f"subscriber not ready: {listener.error}")
    before = process_sample(pid)
    start = time.perf_counter()
    request(port, "POST", f"/api/rooms/{rid}/messages", {
        "text": "PERF_JSON " + json.dumps({"tokens": tokens, "delay_us": delay_us, "tools": 4})})
    peak_rss = before["rss_kib"]
    while not all(listener.done.is_set() for listener in listeners):
        if time.perf_counter() - start > 60:
            raise TimeoutError("synthetic turn did not finish")
        current = process_sample(pid)["rss_kib"]
        if current is not None:
            peak_rss = max(peak_rss or 0, current)
        time.sleep(.01)
    elapsed = (time.perf_counter() - start) * 1000
    after = process_sample(pid)
    expected = "".join(f"piece-{i:04d}: 검증용 **stream** data.\n" for i in range(tokens))
    for listener in listeners:
        assert listener.error is None, listener.error
        assert len(listener.completed) == participants
        for mid, msg in listener.completed.items():
            assert msg["status"] == "done", msg
            # Existing room completion semantics trim surrounding whitespace.
            assert msg["text"] == expected.strip(), "final body differs"
            assert "".join(listener.deltas[mid]) == expected, "SSE lost/reordered text"
            assert listener.trace_indices[mid] == list(range(1, msg["trace"] + 1)), "SSE trace gap"
    snapshot = request(port, "GET", f"/api/rooms/{rid}?limit=200")
    answers = [m for m in snapshot["messages"] if m["kind"] == "agent"]
    assert len(answers) == participants
    for msg in answers:
        trace = request(port, "GET", f"/api/rooms/{rid}/trace/{msg['id']}")
        assert len(trace) == msg["trace"], "persisted trace lost events"
        assert "".join(e.get("text", "") for e in trace if e["kind"] == "text_delta") == expected
    durations = [(when - start) * 1000 for when in listeners[0].complete_at]
    result = {"wall_ms": elapsed, "completion_p50_ms": statistics.median(durations),
              "completion_p95_ms": quantile(durations, .95),
              "server_cpu_ms": None if before["cpu_ms"] is None else after["cpu_ms"] - before["cpu_ms"],
              "server_peak_rss_kib": peak_rss, "sse_events_per_subscriber": listeners[0].events,
              "verified_replies": len(answers), "verified_deltas_per_subscriber": participants * tokens}
    request(port, "DELETE", f"/api/rooms/{rid}")
    return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", type=Path, required=True)
    parser.add_argument("--fixture", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--participants", type=int, nargs="+", default=[1, 4, 16, 32])
    parser.add_argument("--subscribers", type=int, default=4)
    parser.add_argument("--samples", type=int, default=5)
    parser.add_argument("--tokens", type=int, default=128)
    parser.add_argument("--delay-us", type=int, default=0)
    args = parser.parse_args()
    assert all(1 <= n <= 32 for n in args.participants)
    assert 1 <= args.subscribers <= 32 and args.samples >= 1
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        port = sock.getsockname()[1]
    with tempfile.TemporaryDirectory(prefix="roundtable-perf-") as directory:
        root = Path(directory)
        missing = str(root / "no-provider")
        command = [str(args.binary.resolve()), "-addr", f"127.0.0.1:{port}", "-data", str(root / "data"),
                   "-claude", str(args.fixture.resolve()), "-codex", missing, "-agy", missing, "-opencode", missing]
        with (root / "server.log").open("w+") as log:
            process = subprocess.Popen(command, stdout=log, stderr=log,
                                       env={**os.environ, "GOMAXPROCS": "4"})
            try:
                for _ in range(200):
                    try:
                        request(port, "GET", "/api/config")
                        break
                    except OSError:
                        if process.poll() is not None:
                            log.seek(0)
                            raise RuntimeError(log.read())
                        time.sleep(.025)
                else:
                    raise TimeoutError("server startup")
                report = {"platform": platform.platform(), "python": platform.python_version(),
                          "gomaxprocs": 4, "subscribers": args.subscribers, "tokens_per_participant": args.tokens,
                          "delay_us": args.delay_us, "paid_model_calls": 0, "cases": []}
                for participants in args.participants:
                    run_sample(port, process.pid, participants, args.subscribers, args.tokens, args.delay_us)
                    idle_before = process_sample(process.pid)
                    rows = [run_sample(port, process.pid, participants, args.subscribers, args.tokens, args.delay_us)
                            for _ in range(args.samples)]
                    time.sleep(.1)  # allow HTTP disconnect cleanup before the fd observation
                    idle_after = process_sample(process.pid)
                    metrics = [key for key in rows[0] if rows[0][key] is not None and not key.startswith("verified")]
                    summary = {key: {"median": statistics.median([r[key] for r in rows]),
                                     "min": min(r[key] for r in rows), "max": max(r[key] for r in rows)}
                               for key in metrics}
                    report["cases"].append({"participants": participants, "idle_before": idle_before,
                                            "idle_after": idle_after, "summary": summary, "samples": rows})
                    print(json.dumps({"participants": participants, "summary": summary}), flush=True)
                args.output.parent.mkdir(parents=True, exist_ok=True)
                args.output.write_text(json.dumps(report, ensure_ascii=False, indent=2) + "\n")
            finally:
                process.terminate()
                try:
                    process.wait(timeout=15)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait()


if __name__ == "__main__":
    main()
