#!/usr/bin/env python3
"""Drive an MCP stdio server with a list of tools/call requests; save each
result's text content to <outdir>/<label>.txt and print byte sizes.

Usage: mcpbench.py '<cmd>|<arg1>|<arg2>...' <outdir> <calls.json>
  e.g. mcpbench.py 'lightspeed|mcp' /tmp/out ls_calls.json
       mcpbench.py '<another-server>|<args>' /tmp/out other_calls.json

See README.md in this directory for how to generate <calls.json> (calls.py)
and how to read the output this prints.
"""
import json, subprocess, sys, os, threading, time

cmd, outdir, calls_path = sys.argv[1].split("|"), sys.argv[2], sys.argv[3]
calls = json.load(open(calls_path))
os.makedirs(outdir, exist_ok=True)
p = subprocess.Popen(cmd, stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                     stderr=subprocess.DEVNULL, text=True, bufsize=1)

def send(m): p.stdin.write(json.dumps(m) + "\n"); p.stdin.flush()
def recv(want):
    while True:
        line = p.stdout.readline()
        if not line: raise SystemExit("server closed")
        try: m = json.loads(line)
        except Exception: continue
        if m.get("id") == want: return m

send({"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"bench","version":"0"}}})
recv(0)
send({"jsonrpc":"2.0","method":"notifications/initialized"})
for i, c in enumerate(calls, 1):
    t0 = time.time()
    send({"jsonrpc":"2.0","id":i,"method":"tools/call","params":{"name":c["tool"],"arguments":c["args"]}})
    m = recv(i); dt = time.time() - t0
    if "error" in m:
        text = json.dumps(m["error"])
    else:
        text = "".join(x.get("text","") for x in m["result"].get("content",[]))
    open(os.path.join(outdir, c["label"] + ".txt"), "w").write(text)
    print(f'{c["label"]:<18} {len(text.encode()):>7} bytes  {dt*1000:6.0f} ms  err={"error" in m or (m.get("result") or {}).get("isError", False)}')
p.stdin.close(); p.wait(timeout=10)
