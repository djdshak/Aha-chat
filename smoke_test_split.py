import asyncio
import json
import time
import itertools
import websockets

BASE = "ws://127.0.0.1:8080/v1/ws?token=dev-token-"

_id = itertools.count(1)
def next_msg_id(prefix="m"):
    return f"{prefix}{int(time.time())}{next(_id)}"

async def recv_until(ws, predicate, timeout=2.0, label=""):
    end = time.time() + timeout
    seen = []
    while time.time() < end:
        try:
            raw = await asyncio.wait_for(ws.recv(), timeout=end - time.time())
        except asyncio.TimeoutError:
            break
        msg = json.loads(raw)
        seen.append(msg)
        print(f"[{label}] <<< {msg}")
        if predicate(msg):
            return msg, seen
    raise AssertionError(f"timeout waiting for expected message on {label}, seen={seen}")

async def connect_user(username):
    url = BASE + username
    ws = await websockets.connect(url)
    print(f"[{username}] connected -> {url}")
    # 等待欢迎 info
    await recv_until(
        ws,
        lambda m: m.get("type") == "info" and username in m.get("text", ""),
        label=username,
    )
    return ws

async def test_1v1_ack_and_delivery():
    print("\n=== test_1v1_ack_and_delivery ===")
    alice = await connect_user("alice")
    bob = await connect_user("bob")

    try:
        mid = next_msg_id("t")
        payload = {"type": "msg", "msg_id": mid, "to": "bob", "text": "hi bob after split"}
        print("[alice] >>>", payload)
        await alice.send(json.dumps(payload))

        # Alice: server_received ACK
        await recv_until(
            alice,
            lambda m: m.get("type") == "ack" and m.get("ack") == "server_received" and m.get("msg_id") == mid,
            label="alice",
        )

        # Alice: self echo
        await recv_until(
            alice,
            lambda m: m.get("type") == "msg" and m.get("msg_id") == mid and m.get("from") == "alice" and m.get("to") == "bob",
            label="alice",
        )

        # Alice: delivered ACK
        await recv_until(
            alice,
            lambda m: m.get("type") == "ack" and m.get("ack") == "delivered" and m.get("msg_id") == mid,
            label="alice",
        )

        # Bob: receive msg
        await recv_until(
            bob,
            lambda m: m.get("type") == "msg" and m.get("msg_id") == mid and m.get("from") == "alice" and m.get("to") == "bob",
            label="bob",
        )

        print("PASS: 1v1 ACK + delivery")
    finally:
        await alice.close()
        await bob.close()

async def test_offline_error():
    print("\n=== test_offline_error ===")
    alice = await connect_user("alice")
    bob = await connect_user("bob")

    try:
        # 先关闭 bob
        await bob.close()
        print("[bob] closed")

        await asyncio.sleep(0.2)  # 给服务器一点时间处理 unregister

        mid = next_msg_id("t")
        payload = {"type": "msg", "msg_id": mid, "to": "bob", "text": "are you offline now?"}
        print("[alice] >>>", payload)
        await alice.send(json.dumps(payload))

        # server_received
        await recv_until(
            alice,
            lambda m: m.get("type") == "ack" and m.get("ack") == "server_received" and m.get("msg_id") == mid,
            label="alice",
        )

        # self echo
        await recv_until(
            alice,
            lambda m: m.get("type") == "msg" and m.get("msg_id") == mid,
            label="alice",
        )

        # offline error
        await recv_until(
            alice,
            lambda m: m.get("type") == "error" and m.get("msg_id") == mid and "user offline: bob" in m.get("text", ""),
            label="alice",
        )

        print("PASS: offline error")
    finally:
        await alice.close()

async def test_validation_errors():
    print("\n=== test_validation_errors ===")
    alice = await connect_user("alice")
    try:
        # missing msg_id
        bad1 = {"type": "msg", "to": "bob", "text": "no id"}
        print("[alice] >>>", bad1)
        await alice.send(json.dumps(bad1))
        await recv_until(
            alice,
            lambda m: m.get("type") == "error" and m.get("text") == "missing msg_id",
            label="alice",
        )

        # empty text
        mid = next_msg_id("t")
        bad2 = {"type": "msg", "msg_id": mid, "to": "bob", "text": "   "}
        print("[alice] >>>", bad2)
        await alice.send(json.dumps(bad2))
        await recv_until(
            alice,
            lambda m: m.get("type") == "error" and m.get("msg_id") == mid and m.get("text") == "empty text",
            label="alice",
        )

        # unknown type
        bad3 = {"type": "ping", "msg_id": next_msg_id("t")}
        print("[alice] >>>", bad3)
        await alice.send(json.dumps(bad3))
        await recv_until(
            alice,
            lambda m: m.get("type") == "error" and m.get("text") == "unknown type",
            label="alice",
        )

        print("PASS: validation errors")
    finally:
        await alice.close()

async def main():
    await test_1v1_ack_and_delivery()
    await test_offline_error()
    await test_validation_errors()
    print("\nALL PASS ✅")

if __name__ == "__main__":
    asyncio.run(main())