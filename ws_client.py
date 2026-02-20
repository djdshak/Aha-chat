import asyncio, json, sys
import websockets

URL = sys.argv[1] if len(sys.argv) > 1 else "ws://127.0.0.1:8080/v1/ws?token=dev-token-yufei"

async def reader(ws):
    async for msg in ws:
        print("<<<", msg)

async def writer(ws):
    loop = asyncio.get_event_loop()
    while True:
        line = await loop.run_in_executor(None, sys.stdin.readline)
        if not line:
            break
        line = line.strip()
        if not line:
            continue
        # 允许你直接输入文本，它会包装成 {"type":"msg","text":...}
        if line.startswith("{"):
            await ws.send(line)
        else:
            await ws.send(json.dumps({"type":"msg","text": line}))

async def main():
    async with websockets.connect(URL) as ws:
        print("connected:", URL)
        await asyncio.gather(reader(ws), writer(ws))

asyncio.run(main())
PY