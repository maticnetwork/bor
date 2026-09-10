#!/usr/bin/env python3
"""Exercise stuck-tx rebroadcast on a dedicated Kurtosis devnet."""

import argparse
import datetime
import json
import pathlib
import re
import signal
import subprocess
import time
import urllib.parse
import urllib.error
import urllib.request


def command(*args, timeout=60):
    result = subprocess.run(args, capture_output=True, text=True, timeout=timeout)
    if result.returncode:
        raise RuntimeError(f"{args[0]} failed: {result.stderr or result.stdout}")
    return result.stdout


def rpc(url, method, params=None):
    body = json.dumps({"jsonrpc": "2.0", "id": 1, "method": method, "params": params or []})
    request = urllib.request.Request(url, body.encode(), {"Content-Type": "application/json"})
    with urllib.request.urlopen(request, timeout=10) as response:
        result = json.load(response)
    if "error" in result:
        raise RuntimeError(f"{method}: {result['error']}")
    return result["result"]


def interrupted(signum, frame):
    raise KeyboardInterrupt("Test interrupted; restoring network rules")


def service(enclave, name):
    url = command("kurtosis", "port", "print", enclave, name, "rpc").strip()
    ids = command("docker", "ps", "-q").split()
    if not ids:
        raise RuntimeError(f"No running container for {name}")
    candidates = [item for item in json.loads(command("docker", "inspect", *ids))
                  if item["Name"].lstrip("/").startswith(f"{name}--")]
    port = str(urllib.parse.urlparse(url).port)
    matches = [item for item in candidates if any(
        binding["HostPort"] == port
        for bindings in item["NetworkSettings"]["Ports"].values()
        for binding in (bindings or [])
    )]
    if len(matches) != 1:
        raise RuntimeError(f"Expected one container matching {name} and RPC port {port}")
    item = matches[0]
    labels = item["Config"]["Labels"]
    return {"name": name, "url": url, "id": item["Id"],
            "ip": labels["com.kurtosistech.private-ip"],
            "enclave": labels["com.kurtosistech.enclave-id"], "image": item["Image"]}


class Test:
    def __init__(self, args):
        self.args = args
        self.output = pathlib.Path(args.artifacts)
        self.output.mkdir(parents=True, exist_ok=True)
        self.target = service(args.enclave, args.service)
        self.producers = [service(args.enclave, name) for name in args.producers]
        if any(node["enclave"] != self.target["enclave"] for node in self.producers):
            raise RuntimeError("All test services must belong to the same enclave")
        self.start = datetime.datetime.now(datetime.timezone.utc).isoformat()
        self.tx = None
        self.shaped = []
        self.fees = []
        self.removed_peer = None
        self.summary = {"enclave": args.enclave, "target": self.target,
                        "producers": self.producers, "started": self.start}

    def logs(self):
        result = subprocess.run(["docker", "logs", "--since", self.start, self.target["id"]],
                                capture_output=True, text=True, timeout=20, check=True)
        logs = result.stdout + result.stderr
        (self.output / "target.log").write_text(logs)
        return logs

    def sample(self, phase):
        url = self.target["url"]
        head = int(rpc(url, "eth_blockNumber"), 16)
        reference = max(int(rpc(node["url"], "eth_blockNumber"), 16) for node in self.producers)
        logs = self.logs()
        sample = {"time": time.time(), "phase": phase, "head": head, "reference": reference,
                  "lag": reference - head, "peers": int(rpc(url, "net_peerCount"), 16),
                  "syncing": rpc(url, "eth_syncing"),
                  "identified": logs.count("Identified stuck transactions for rebroadcast"),
                  "rebroadcast": logs.count("Rebroadcast stuck transactions")}
        if self.tx:
            tx = rpc(url, "eth_getTransactionByHash", [self.tx])
            if tx is None or tx["blockHash"] is not None:
                raise RuntimeError("Fixture transaction must remain pending throughout the test")
        with (self.output / "samples.jsonl").open("a") as output:
            output.write(json.dumps(sample) + "\n")
        print(json.dumps(sample), flush=True)
        return sample

    def wait(self, phase, condition):
        deadline = time.monotonic() + self.args.timeout
        last_error = None
        while time.monotonic() < deadline:
            try:
                sample = self.sample(phase)
            except (OSError, urllib.error.URLError) as error:
                last_error = error
                time.sleep(1)
                continue
            if condition(sample):
                return sample
            time.sleep(1)
        if last_error is not None:
            raise RuntimeError(f"Timed out waiting for {phase}: {last_error}")
        raise RuntimeError(f"Timed out waiting for {phase}")

    def seed(self):
        url = self.target["url"]
        key_log = command("kurtosis", "service", "logs", self.args.enclave,
                          "l2-tx-spammer", "--all", "--match", "PRIVATE_KEY")
        match = re.search(r"PRIVATE_KEY:\s*(0x[0-9a-fA-F]+)", key_log)
        if match is None:
            raise RuntimeError("Could not find the devnet transaction-spammer key")
        private_key = match[1]
        sender = command("docker", "run", "--rm", "--add-host",
                         "host.docker.internal:host-gateway", "--entrypoint", "cast",
                         self.args.cast_image, "wallet", "address", "--private-key",
                         private_key).strip()
        nonce = int(rpc(url, "eth_getTransactionCount", [sender, "pending"]), 16)
        docker_url = url.replace("127.0.0.1", "host.docker.internal", 1)
        self.tx = command("docker", "run", "--rm", "--add-host",
                          "host.docker.internal:host-gateway", "--entrypoint", "cast",
                          self.args.cast_image, "send", "--async", "--rpc-url", docker_url,
                          "--private-key", private_key, "--legacy", "--nonce", str(nonce),
                          "--gas-price", "30000000000",
                          "0x000000000000000000000000000000000000dEaD", "--value", "1").strip()
        self.summary["transaction"] = self.tx

    def gas_price(self, node, price):
        result = command("docker", "exec", node["id"], "bor", "attach", "/var/lib/bor/bor.ipc",
                         "--exec", f"miner.setGasPrice({price})")
        if result.strip() != "true":
            raise RuntimeError(f"Could not set fixture gas price for {node['name']}: {result}")

    def peer_enode(self, node):
        return command("docker", "exec", node["id"], "bor", "attach", "/var/lib/bor/bor.ipc",
                       "--exec", "admin.nodeInfo.enode").strip().strip('"')

    def set_peer(self, action, enode):
        result = command("docker", "exec", self.target["id"], "bor", "attach",
                         "/var/lib/bor/bor.ipc", "--exec", f'admin.{action}Peer("{enode}")')
        if result.strip() != "true":
            raise RuntimeError(f"Could not {action} fixture peer: {result}")

    def retain_pending(self):
        for node in self.producers:
            config = command("docker", "exec", node["id"], "cat", "/etc/bor/config.toml")
            match = re.search(r'^\s*gasprice\s*=\s*"(\d+)"', config, re.MULTILINE)
            if match is None:
                raise RuntimeError("Fixture needs an explicit validator gas price to restore")
            self.fees.append((node, int(match[1])))
            self.gas_price(node, 1_000_000_000_000)

    def restore_fees(self):
        for node, price in self.fees[:]:
            self.gas_price(node, price)
            self.fees.remove((node, price))

    def create_gap(self):
        self.removed_peer = self.peer_enode(self.producers[0])
        self.set_peer("remove", self.removed_peer)
        self.wait("build-gap", lambda s: s["peers"] == 0
                  and s["lag"] >= self.args.initial_gap)

    def reconnect(self):
        if self.removed_peer is None:
            return
        self.set_peer("add", self.removed_peer)
        self.removed_peer = None

    def tc(self, node, *args):
        return command("docker", "run", "--rm", "--network", f"container:{node['id']}",
                       "--cap-add", "NET_ADMIN", "--entrypoint", "tc", self.args.tc_image, *args)

    def partition(self):
        for node in self.producers:
            self.tc(node, "qdisc", "add", "dev", "eth0", "root", "handle", "1:",
                    "prio", "bands", "3", "priomap", *(["0"] * 16))
            self.shaped.append(node)
            self.tc(node, "qdisc", "add", "dev", "eth0", "parent", "1:3", "handle", "30:",
                    "netem", "loss", "100%")
            for port in ["sport", "dport"]:
                self.tc(node, "filter", "add", "dev", "eth0", "protocol", "ip", "parent", "1:",
                        "prio", "1", "u32", "match", "ip", "dst", self.target["ip"] + "/32",
                        "match", "ip", "protocol", "6", "0xff", "match", "ip", port,
                        "30303", "0xffff", "flowid", "1:3")

    def impair(self):
        for node in self.shaped:
            self.tc(node, "qdisc", "change", "dev", "eth0", "parent", "1:3", "handle", "30:",
                    "netem", "delay", self.args.delay, "rate", self.args.rate, "limit", "10000")

    def restore(self):
        failures = []
        for node in self.shaped[:]:
            try:
                self.tc(node, "qdisc", "del", "dev", "eth0", "root")
                self.shaped.remove(node)
            except (RuntimeError, subprocess.SubprocessError) as error:
                failures.append(str(error))
        if failures:
            raise RuntimeError("Network cleanup failed: " + "; ".join(failures))

    def suppression(self):
        first = self.wait("detect-sync", lambda s: s["lag"] >= self.args.min_lag and s["peers"] > 0)
        start = self.sample("suppressed")
        deadline = time.monotonic() + self.args.window
        last = start
        saw_syncing = start["syncing"] is not False
        while time.monotonic() < deadline:
            last = self.sample("suppressed")
            if last["peers"] == 0:
                raise RuntimeError("Suppression window lost its connected-peer precondition")
            if last["rebroadcast"] != start["rebroadcast"]:
                raise RuntimeError("Out-of-sync node rebroadcast stuck transactions")
            saw_syncing = saw_syncing or last["syncing"] is not False
            if last["lag"] < self.args.min_lag:
                break
            time.sleep(1)
        identified = last["identified"] - start["identified"]
        if identified < 3:
            raise RuntimeError("Need at least three stuck-tx batches to prove suppression")
        if last["head"] <= start["head"]:
            raise RuntimeError("Target block did not advance during suppressed catch-up")
        if not saw_syncing:
            raise RuntimeError("Target never reported an active catch-up sync")
        self.summary["suppression"] = {"first_sync": first, "start": start, "end": last,
                                       "observed_active_sync": saw_syncing,
                                       "identified_batches": identified, "rebroadcast_batches": 0}

    def run(self):
        self.wait("initial-sync", lambda s: s["head"] >= 20 and s["lag"] <= 2
                  and s["peers"] > 0 and s["syncing"] is False)
        self.retain_pending()
        self.seed()
        self.summary["baseline"] = self.wait("baseline", lambda s: s["rebroadcast"] >= 3)
        self.partition()
        self.create_gap()
        self.impair()
        self.reconnect()
        self.suppression()
        self.restore()
        caught_up = self.wait("catch-up", lambda s: s["lag"] <= 2 and s["syncing"] is False)
        self.summary["recovery"] = self.wait(
            "recovery", lambda s: s["lag"] <= 2 and s["peers"] > 0
            and s["rebroadcast"] >= caught_up["rebroadcast"] + 3)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--enclave", required=True)
    parser.add_argument("--artifacts", required=True)
    parser.add_argument("--service", default="l2-el-2-bor-heimdall-v2-rpc")
    parser.add_argument("--producers", nargs="+", default=[
        "l2-el-1-bor-heimdall-v2-validator"])
    parser.add_argument("--delay", default="1500ms")
    parser.add_argument("--rate", default="64kbit")
    parser.add_argument("--window", type=int, default=12)
    parser.add_argument("--min-lag", type=int, default=3)
    parser.add_argument("--initial-gap", type=int, default=20)
    parser.add_argument("--timeout", type=int, default=180)
    parser.add_argument("--tc-image", default="gaiadocker/iproute2:3.3")
    parser.add_argument("--cast-image", default="ghcr.io/foundry-rs/foundry@sha256:0c00cb0bda1ab1b91c9a6bf60f4c76c09c1a8870824b6d4718afbabacf6f9a17")
    args = parser.parse_args()
    if args.window < 8 or args.min_lag < 1 or args.initial_gap < args.min_lag or args.timeout < 1:
        parser.error("window must be >=8 seconds; lag values and timeout must be positive")
    test = Test(args)
    signal.signal(signal.SIGTERM, interrupted)
    try:
        test.run()
        test.summary["result"] = "PASS"
    except (Exception, KeyboardInterrupt) as error:
        test.summary.update(result="FAIL", error=str(error))
        raise
    finally:
        try:
            test.restore()
            test.reconnect()
            test.restore_fees()
        except Exception as error:
            test.summary.update(result="FAIL", cleanup_error=str(error))
            raise
        finally:
            (test.output / "summary.json").write_text(json.dumps(test.summary, indent=2) + "\n")
    print("PASS: rebroadcast enabled when synced, suppressed while behind, restored after catch-up")


if __name__ == "__main__":
    main()
