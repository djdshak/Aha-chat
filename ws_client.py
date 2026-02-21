import asyncio
import json
import sys
import time
import itertools

import websockets

URL = sys.argv[1] if len(sys.argv) > 1 else "ws://127.0.0.1:8080/v1/ws?token=dev-token-yufei"

# 简单本地 msg_id 生成器：m<毫秒时间戳>-<递增序号>
_counter = itertools.count(1)


def next_msg_id() -> str:
    return f"m{int(time.time() * 1000)}-{next(_counter)}"


async def reader(ws):
    try:
        async for msg in ws:
            print("<<<", msg)
    except websockets.ConnectionClosed as e:
        print(f"\n[ws closed] code={e.code} reason={e.reason}")


async def writer(ws):
    loop = asyncio.get_event_loop()
    while True:
        line = await loop.run_in_executor(None, sys.stdin.readline)
        if not line:
            break
        line = line.strip()
        if not line:
            continue

        if line in ("/quit", "/exit"):
            await ws.close()
            break

        # 允许你直接输入原始 JSON（方便测 bad json / 特殊字段）
        if line.startswith("{"):
            await ws.send(line)
            continue

        # 1v1 快捷语法：@bob hi bob
        if line.startswith("@"):
            s = line[1:].strip()
            if not s:
                print("usage: @username message")
                continue
            parts = s.split(None, 1)
            if len(parts) < 2:
                print("usage: @username message")
                continue
            to, text = parts[0].strip(), parts[1].strip()
            if not to or not text:
                print("usage: @username message")
                continue

            payload = {
                "type": "msg",
                "msg_id": next_msg_id(),
                "to": to,
                "text": text,
            }
            await ws.send(json.dumps(payload, ensure_ascii=False))
            print(">>>", payload)
            continue

        # 普通文本：自动包装成广播消息（带 msg_id）
        payload = {
            "type": "msg",
            "msg_id": next_msg_id(),
            "text": line,
        }
        await ws.send(json.dumps(payload, ensure_ascii=False))
        print(">>>", payload)


async def main():
    async with websockets.connect(URL) as ws:
        print("connected:", URL)

        # 比 gather 更稳：任一任务结束就取消另一个（比如 Ctrl+C/连接断开）
        rtask = asyncio.create_task(reader(ws))
        wtask = asyncio.create_task(writer(ws))

        done, pending = await asyncio.wait(
            {rtask, wtask},
            return_when=asyncio.FIRST_COMPLETED,
        )

        for t in pending:
            t.cancel()
        await asyncio.gather(*pending, return_exceptions=True)


if __name__ == "__main__":
    try:
        asyncio.run(main())
    except KeyboardInterrupt:
        print("\nbye")