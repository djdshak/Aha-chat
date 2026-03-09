#!/usr/bin/env python3
# -*- coding: utf-8 -*-

import asyncio
import json
import os
import sys
import time
import itertools
from dataclasses import dataclass
from pathlib import Path
from typing import Dict, List, Optional, Tuple

import aiohttp
import websockets

from prompt_toolkit.application import Application
from prompt_toolkit.key_binding import KeyBindings
from prompt_toolkit.layout import Layout, HSplit
from prompt_toolkit.widgets import TextArea, Frame
from prompt_toolkit.styles import Style

from prompt_toolkit.shortcuts import input_dialog


# ----------------------------
# Config
# ----------------------------

APP_DIR = Path.home() / ".config" / "aha-chat"
CFG_PATH = APP_DIR / "config.json"

DEFAULT_BASE_URL = os.environ.get("AHA_BASE_URL", "https://chat.xyt.app")
HISTORY_LIMIT = 80
SYNC_LIMIT = 200
MAX_BUFFER_PER_PEER = 400


def _now_s() -> int:
    return int(time.time())


_counter = itertools.count(1)
def next_msg_id() -> str:
    return f"m{int(time.time() * 1000)}-{next(_counter)}"


def base_to_ws(base_url: str) -> str:
    # https://x -> wss://x ; http://x -> ws://x
    if base_url.startswith("https://"):
        return "wss://" + base_url[len("https://"):]
    if base_url.startswith("http://"):
        return "ws://" + base_url[len("http://"):]
    # assume https
    return "wss://" + base_url


def ensure_cfg_dir():
    APP_DIR.mkdir(parents=True, exist_ok=True)
    try:
        os.chmod(APP_DIR, 0o700)
    except Exception:
        pass


def load_cfg() -> dict:
    ensure_cfg_dir()
    if not CFG_PATH.exists():
        return {}
    try:
        return json.loads(CFG_PATH.read_text(encoding="utf-8"))
    except Exception:
        return {}


def save_cfg(cfg: dict):
    ensure_cfg_dir()
    tmp = CFG_PATH.with_suffix(".tmp")
    tmp.write_text(json.dumps(cfg, ensure_ascii=False, indent=2), encoding="utf-8")
    tmp.replace(CFG_PATH)
    try:
        os.chmod(CFG_PATH, 0o600)
    except Exception:
        pass


# ----------------------------
# Models / State
# ----------------------------

@dataclass
class ChatMsg:
    msg_id: str
    from_user: str
    to_user: str
    text: str
    ts: int
    status: str = "delivered"  # pending/server_received/delivered/failed


class State:
    def __init__(self):
        self.base_url: str = DEFAULT_BASE_URL
        self.ws_base: str = base_to_ws(self.base_url)
        self.username: str = ""
        self.token: str = ""
        self.expires_at: int = 0

        self.current_peer: str = ""
        self.peers_order: List[str] = []
        self.unread: Dict[str, int] = {}

        # peer -> list[ChatMsg]
        self.buffers: Dict[str, List[ChatMsg]] = {}

        # msg_id -> (peer, index) for quick status updates
        self.msg_index: Dict[str, Tuple[str, int]] = {}

        # sync watermark (db id)
        self.after_id: int = 0

        # WS connection
        self.ws: Optional[websockets.WebSocketClientProtocol] = None
        self.ws_connected: bool = False
        self.stop_flag: bool = False

    def add_peer(self, peer: str):
        peer = peer.strip()
        if not peer:
            return
        if peer not in self.peers_order:
            self.peers_order.insert(0, peer)
        else:
            # move to front
            self.peers_order.remove(peer)
            self.peers_order.insert(0, peer)
        self.unread.setdefault(peer, 0)
        self.buffers.setdefault(peer, [])

    def buffer_append(self, peer: str, m: ChatMsg):
        self.add_peer(peer)
        buf = self.buffers[peer]

        # de-dup by msg_id (important for sync + ws overlap)
        if m.msg_id in self.msg_index:
            # already have it; but status might be better
            p, idx = self.msg_index[m.msg_id]
            if p == peer:
                # upgrade status if needed
                old = buf[idx]
                if old.status != m.status and old.status != "delivered":
                    old.status = m.status
            return

        buf.append(m)
        if len(buf) > MAX_BUFFER_PER_PEER:
            # drop oldest
            drop = len(buf) - MAX_BUFFER_PER_PEER
            for i in range(drop):
                old = buf[i]
                self.msg_index.pop(old.msg_id, None)
            del buf[:drop]

        self.msg_index[m.msg_id] = (peer, len(buf) - 1)

    def update_status(self, msg_id: str, status: str):
        loc = self.msg_index.get(msg_id)
        if not loc:
            return
        peer, idx = loc
        buf = self.buffers.get(peer)
        if not buf or idx >= len(buf):
            return
        buf[idx].status = status


# ----------------------------
# HTTP Client
# ----------------------------

class API:
    def __init__(self, st: State):
        self.st = st
        self.session: Optional[aiohttp.ClientSession] = None

    async def __aenter__(self):
        timeout = aiohttp.ClientTimeout(total=15)
        self.session = aiohttp.ClientSession(timeout=timeout)
        return self

    async def __aexit__(self, exc_type, exc, tb):
        if self.session:
            await self.session.close()

    def _auth_headers(self) -> dict:
        if not self.st.token:
            return {}
        return {"Authorization": f"Bearer {self.st.token}"}

    async def register(self, username: str, password: str) -> dict:
        url = self.st.base_url.rstrip("/") + "/v1/register"
        async with self.session.post(url, json={"username": username, "password": password}) as resp:
            txt = await resp.text()
            try:
                return json.loads(txt)
            except Exception:
                return {"error": f"http_{resp.status}", "details": txt}

    async def login(self, username: str, password: str) -> dict:
        url = self.st.base_url.rstrip("/") + "/v1/login"
        async with self.session.post(url, json={"username": username, "password": password}) as resp:
            txt = await resp.text()
            try:
                return json.loads(txt)
            except Exception:
                return {"error": f"http_{resp.status}", "details": txt}

    async def history(self, peer: str, limit: int = HISTORY_LIMIT) -> dict:
        url = self.st.base_url.rstrip("/") + "/v1/history"
        params = {"peer": peer, "limit": str(limit)}
        async with self.session.get(url, params=params, headers=self._auth_headers()) as resp:
            txt = await resp.text()
            if resp.status == 401:
                return {"error": "unauthorized", "details": txt}
            try:
                return json.loads(txt)
            except Exception:
                return {"error": f"http_{resp.status}", "details": txt}

    async def sync(self, after_id: int, limit: int = SYNC_LIMIT) -> dict:
        url = self.st.base_url.rstrip("/") + "/v1/sync"
        params = {"after_id": str(after_id), "limit": str(limit)}
        async with self.session.get(url, params=params, headers=self._auth_headers()) as resp:
            txt = await resp.text()
            if resp.status == 401:
                return {"error": "unauthorized", "details": txt}
            try:
                return json.loads(txt)
            except Exception:
                return {"error": f"http_{resp.status}", "details": txt}


# ----------------------------
# TUI
# ----------------------------

class TUI:
    def __init__(self, st: State):
        self.st = st

        self.chat_area = TextArea(
            text="",
            focusable=False,
            scrollbar=True,
            wrap_lines=True,
            read_only=True,
        )
        self.status_area = TextArea(
            text="",
            height=1,
            focusable=False,
            read_only=True,
        )
        self.input_area = TextArea(
            height=1,
            prompt="> ",
            multiline=False,
            wrap_lines=False,
        )

        kb = KeyBindings()

        @kb.add("c-c")
        @kb.add("c-q")
        def _(event):
            self.st.stop_flag = True
            event.app.exit()

        @kb.add("c-n")
        def _(event):
            # next peer
            if not self.st.peers_order:
                return
            if not self.st.current_peer:
                asyncio.create_task(self.switch_peer(self.st.peers_order[0]))
                return
            try:
                i = self.st.peers_order.index(self.st.current_peer)
            except ValueError:
                i = -1
            nxt = self.st.peers_order[(i + 1) % len(self.st.peers_order)]
            asyncio.create_task(self.switch_peer(nxt))

        @kb.add("c-p")
        def _(event):
            # prev peer
            if not self.st.peers_order:
                return
            if not self.st.current_peer:
                asyncio.create_task(self.switch_peer(self.st.peers_order[0]))
                return
            try:
                i = self.st.peers_order.index(self.st.current_peer)
            except ValueError:
                i = 0
            prv = self.st.peers_order[(i - 1) % len(self.st.peers_order)]
            asyncio.create_task(self.switch_peer(prv))

        @kb.add("enter")
        def _(event):
            line = self.input_area.text.strip()
            self.input_area.text = ""
            asyncio.create_task(self.handle_line(line))

        self.app = Application(
            layout=Layout(
                HSplit(
                    [
                        Frame(self.chat_area, title="aha-chat (Terminal)"),
                        Frame(self.status_area, title="status"),
                        Frame(self.input_area, title="input  (/help)"),
                    ]
                )
            ),
            key_bindings=kb,
            full_screen=True,
            style=Style.from_dict(
                {
                    "frame.border": "#888888",
                }
            ),
        )

    def set_status(self, s: str):
        self.status_area.text = s
        self.app.invalidate()

    def append_chat(self, s: str):
        # Keep cursor at end (auto-scroll)
        if self.chat_area.text:
            self.chat_area.text += "\n" + s
        else:
            self.chat_area.text = s
        # move cursor to end to keep newest visible
        try:
            self.chat_area.buffer.cursor_position = len(self.chat_area.text)
        except Exception:
            pass
        self.app.invalidate()

    def render_current(self):
        peer = self.st.current_peer
        if not peer:
            self.chat_area.text = (
                "No peer selected.\n"
                "Use: /chat <username>  OR  @username <message>\n"
                "Ctrl+N / Ctrl+P to cycle peers once you have some.\n"
            )
            self.app.invalidate()
            return

        buf = self.st.buffers.get(peer, [])
        lines = []
        unread = self.st.unread.get(peer, 0)
        header = f"[chatting with {peer}]"
        if unread > 0:
            header += f"  (unread cleared: {unread})"
        lines.append(header)
        lines.append("-" * max(10, len(header)))

        for m in buf[-MAX_BUFFER_PER_PEER:]:
            direction = ">>" if m.from_user == self.st.username else "<<"
            status = ""
            if m.from_user == self.st.username:
                status = f" [{m.status}]"
            ts = time.strftime("%H:%M:%S", time.localtime(m.ts))
            lines.append(f"{direction} {ts} {m.from_user}: {m.text}{status}")

        self.chat_area.text = "\n".join(lines)
        try:
            self.chat_area.buffer.cursor_position = len(self.chat_area.text)
        except Exception:
            pass
        self.app.invalidate()

    async def switch_peer(self, peer: str):
        peer = peer.strip()
        if not peer:
            return
        self.set_status(f"loading history with {peer} ...")
        self.st.current_peer = peer
        self.st.unread[peer] = 0
        self.st.add_peer(peer)

        # fetch history
        async with API(self.st) as api:
            resp = await api.history(peer, HISTORY_LIMIT)
        if "error" in resp:
            self.append_chat(f"[error] history: {resp.get('details')}")
            self.set_status("history failed")
            return

        items = resp.get("items", [])
        # Replace buffer for this peer with server history (keeps local order)
        self.st.buffers[peer] = []
        # Rebuild index entries for this peer
        # (safe: we only remove ones belonging to this peer)
        for mid, (p, _) in list(self.st.msg_index.items()):
            if p == peer:
                self.st.msg_index.pop(mid, None)

        for it in items:
            m = ChatMsg(
                msg_id=it["msg_id"],
                from_user=it["from_user"],
                to_user=it["to_user"],
                text=it["text"],
                ts=int(it.get("created_at", _now_s())),
                status="delivered",
            )
            # determine peer
            self.st.buffer_append(peer, m)

        self.render_current()
        self.set_status(f"ready (peer={peer})")

    async def ensure_login(self):
        cfg = load_cfg()
        if cfg.get("base_url"):
            self.st.base_url = cfg["base_url"]
        self.st.ws_base = base_to_ws(self.st.base_url)

        self.st.username = cfg.get("username", "")
        self.st.token = cfg.get("token", "")
        self.st.expires_at = int(cfg.get("expires_at", 0) or 0)
        self.st.after_id = int(cfg.get("after_id", 0) or 0)
        self.st.peers_order = cfg.get("peers_order", []) or []
        self.st.current_peer = cfg.get("current_peer", "") or ""
        self.st.unread = cfg.get("unread", {}) or {}

        # Validate token quickly
        if self.st.token:
            self.set_status("validating saved token ...")
            async with API(self.st) as api:
                resp = await api.sync(after_id=self.st.after_id, limit=1)
            if resp.get("error") == "unauthorized":
                self.st.token = ""
                self.st.expires_at = 0
                self.set_status("saved token expired/invalid. please login.")
            else:
                self.set_status("token ok.")
                return

        # Ask base_url (optional)
        self.append_chat(f"Server base URL (default: {DEFAULT_BASE_URL}).")
        self.append_chat("Type: /base https://chat.xyt.app   (or press Enter and continue)")
        self.append_chat("Then: /login <username>  (you will be prompted for password)")

    def persist_cfg(self):
        cfg = load_cfg()
        cfg["base_url"] = self.st.base_url
        cfg["username"] = self.st.username
        cfg["token"] = self.st.token
        cfg["expires_at"] = self.st.expires_at
        cfg["after_id"] = self.st.after_id
        cfg["peers_order"] = self.st.peers_order
        cfg["current_peer"] = self.st.current_peer
        cfg["unread"] = self.st.unread
        save_cfg(cfg)

    async def do_login(self, username: str, password: str):
        self.set_status("logging in ...")
        async with API(self.st) as api:
            resp = await api.login(username, password)
        if resp.get("token"):
            self.st.username = resp.get("username", username)
            self.st.token = resp["token"]
            self.st.expires_at = int(resp.get("expires_at", 0) or 0)
            self.persist_cfg()
            self.append_chat(f"[ok] logged in as {self.st.username} (expires_at={self.st.expires_at})")
            self.set_status("login ok")
        else:
            self.append_chat(f"[error] login failed: {resp}")
            self.set_status("login failed")

    async def do_register(self, username: str, password: str):
        self.set_status("registering ...")
        async with API(self.st) as api:
            resp = await api.register(username, password)
        if resp.get("ok") is True:
            self.append_chat("[ok] registered. now /login")
            self.set_status("register ok")
        else:
            self.append_chat(f"[error] register failed: {resp}")
            self.set_status("register failed")

    async def do_sync(self):
        if not self.st.token:
            self.append_chat("[hint] /login first.")
            return
        self.set_status(f"syncing after_id={self.st.after_id} ...")
        async with API(self.st) as api:
            resp = await api.sync(after_id=self.st.after_id, limit=SYNC_LIMIT)
        if resp.get("error") == "unauthorized":
            self.append_chat("[error] unauthorized. token expired? /login again.")
            self.st.token = ""
            self.persist_cfg()
            self.set_status("unauthorized")
            return
        if "error" in resp and not resp.get("items"):
            self.append_chat(f"[error] sync failed: {resp}")
            self.set_status("sync failed")
            return

        items = resp.get("items", [])
        next_after = int(resp.get("next_after_id", self.st.after_id) or self.st.after_id)

        # Apply items (these are to_user=self)
        new_count = 0
        for it in items:
            from_u = it["from_user"]
            to_u = it["to_user"]
            peer = from_u if from_u != self.st.username else to_u
            m = ChatMsg(
                msg_id=it["msg_id"],
                from_user=from_u,
                to_user=to_u,
                text=it["text"],
                ts=int(it.get("created_at", _now_s())),
                status="delivered",
            )
            before = len(self.st.buffers.get(peer, []))
            self.st.buffer_append(peer, m)
            after = len(self.st.buffers.get(peer, []))
            if after > before:
                new_count += 1
                if peer != self.st.current_peer:
                    self.st.unread[peer] = self.st.unread.get(peer, 0) + 1

        self.st.after_id = max(self.st.after_id, next_after)
        self.persist_cfg()
        if self.st.current_peer:
            self.render_current()
        self.set_status(f"sync ok (+{new_count}, after_id={self.st.after_id})")

    async def ws_connect_loop(self):
        # persistent WS loop with reconnect
        while not self.st.stop_flag:
            if not self.st.token:
                await asyncio.sleep(0.3)
                continue

            ws_url = self.st.ws_base.rstrip("/") + "/v1/ws?token=" + self.st.token
            try:
                self.set_status(f"ws connecting ...")
                async with websockets.connect(ws_url, ping_interval=20, ping_timeout=20) as ws:
                    self.st.ws = ws
                    self.st.ws_connected = True
                    self.set_status("ws connected")
                    # after connect: do one sync
                    await self.do_sync()

                    async for raw in ws:
                        try:
                            msg = json.loads(raw)
                        except Exception:
                            self.append_chat(f"[ws] <<< {raw}")
                            continue
                        await self.handle_ws_msg(msg)

            except Exception as e:
                self.st.ws = None
                self.st.ws_connected = False
                self.set_status(f"ws disconnected: {type(e).__name__}: {e}")
                await asyncio.sleep(1.5)

    async def handle_ws_msg(self, msg: dict):
        t = msg.get("type", "")
        if t == "info":
            self.append_chat(f"[info] {msg.get('text','')}")
            return

        if t == "msg":
            msg_id = msg.get("msg_id", "")
            from_u = msg.get("from", "")
            to_u = msg.get("to", "")
            text = msg.get("text", "")
            ts = int(msg.get("ts", _now_s()))
            if not from_u or not to_u:
                return
            peer = from_u if from_u != self.st.username else to_u
            m = ChatMsg(msg_id=msg_id, from_user=from_u, to_user=to_u, text=text, ts=ts, status="delivered")
            before = len(self.st.buffers.get(peer, []))
            self.st.buffer_append(peer, m)
            after = len(self.st.buffers.get(peer, []))
            if after > before:
                if peer != self.st.current_peer:
                    self.st.unread[peer] = self.st.unread.get(peer, 0) + 1
                else:
                    self.st.unread[peer] = 0
                    self.render_current()
                self.persist_cfg()
            else:
                # already have it (dedup)
                if peer == self.st.current_peer:
                    self.render_current()
            return

        if t == "ack":
            msg_id = msg.get("msg_id", "")
            ack = msg.get("ack", "")
            if not msg_id:
                return
            if ack == "server_received":
                self.st.update_status(msg_id, "server_received")
            elif ack == "delivered":
                self.st.update_status(msg_id, "delivered")
            if self.st.current_peer:
                self.render_current()
            return

        if t == "error":
            msg_id = msg.get("msg_id", "")
            text = msg.get("text", msg.get("error", "error"))
            if msg_id:
                self.st.update_status(msg_id, "failed")
                if self.st.current_peer:
                    self.render_current()
            self.append_chat(f"[error] {text}")
            return

        # fallback
        self.append_chat(f"[ws] {msg}")

    async def ws_send_msg(self, to_user: str, text: str):
        if not self.st.ws or not self.st.ws_connected:
            self.append_chat("[error] ws not connected. try again or wait reconnect.")
            return
        to_user = to_user.strip()
        text = text.strip()
        if not to_user or not text:
            return

        mid = next_msg_id()
        ts = _now_s()

        # optimistic UI append
        m = ChatMsg(msg_id=mid, from_user=self.st.username, to_user=to_user, text=text, ts=ts, status="pending")
        self.st.buffer_append(to_user, m)
        self.st.unread[to_user] = 0
        self.st.current_peer = to_user if self.st.current_peer in ("", to_user) else self.st.current_peer
        self.persist_cfg()
        if self.st.current_peer == to_user:
            self.render_current()

        payload = {"type": "msg", "msg_id": mid, "to": to_user, "text": text}
        try:
            await self.st.ws.send(json.dumps(payload, ensure_ascii=False))
        except Exception as e:
            self.st.update_status(mid, "failed")
            if self.st.current_peer == to_user:
                self.render_current()
            self.append_chat(f"[error] send failed: {e}")

    async def handle_line(self, line: str):
        if not line:
            return

        if line.startswith("/"):
            await self.handle_command(line)
            return

        # quick @peer msg
        if line.startswith("@"):
            s = line[1:].strip()
            parts = s.split(None, 1)
            if len(parts) < 2:
                self.append_chat("usage: @username message")
                return
            peer, text = parts[0], parts[1]
            await self.ws_send_msg(peer, text)
            return

        # if current peer set, send to it
        if self.st.current_peer:
            await self.ws_send_msg(self.st.current_peer, line)
            return

        self.append_chat("No peer selected. Use /chat <username> or @username <message>.")

    async def handle_command(self, line: str):
        parts = line.strip().split()
        cmd = parts[0].lower()

        if cmd in ("/quit", "/exit"):
            self.st.stop_flag = True
            self.app.exit()
            return

        if cmd == "/help":
            self.append_chat(
                "Commands:\n"
                "  /base <https://chat.xyt.app>      set server base url\n"
                "  /register <user>                 register (will prompt password)\n"
                "  /login <user>                    login (will prompt password)\n"
                "  /chat <peer>                     switch peer + load history\n"
                "  /peers                           list peers & unread\n"
                "  /history [n]                     reload history for current peer\n"
                "  /sync                            pull offline inbox\n"
                "  /logout                          clear token\n"
                "  /quit                            exit\n"
                "Tips:\n"
                "  @bob hello   (quick send)\n"
                "  Ctrl+N / Ctrl+P cycle peers\n"
            )
            return

        if cmd == "/base":
            if len(parts) < 2:
                self.append_chat(f"current base_url: {self.st.base_url}")
                return
            self.st.base_url = parts[1].strip().rstrip("/")
            self.st.ws_base = base_to_ws(self.st.base_url)
            self.persist_cfg()
            self.append_chat(f"[ok] base_url set to {self.st.base_url}")
            return

        if cmd == "/logout":
            self.st.token = ""
            self.st.expires_at = 0
            self.persist_cfg()
            self.append_chat("[ok] logged out (token cleared). use /login <user>")
            return

        if cmd == "/peers":
            if not self.st.peers_order:
                self.append_chat("(no peers yet)")
                return
            lines = ["Peers (most recent first):"]
            for p in self.st.peers_order:
                u = self.st.unread.get(p, 0)
                cur = " <==" if p == self.st.current_peer else ""
                lines.append(f"  {p}  unread={u}{cur}")
            self.append_chat("\n".join(lines))
            return

        if cmd == "/chat":
            if len(parts) < 2:
                self.append_chat("usage: /chat <peer>")
                return
            await self.switch_peer(parts[1])
            return

        if cmd == "/history":
            if not self.st.current_peer:
                self.append_chat("no current peer. use /chat <peer>")
                return
            n = HISTORY_LIMIT
            if len(parts) >= 2:
                try:
                    n = max(10, min(500, int(parts[1])))
                except Exception:
                    n = HISTORY_LIMIT
            # reuse switch_peer but with different limit: simplest = call history manually
            peer = self.st.current_peer
            self.set_status(f"loading history with {peer} ...")
            async with API(self.st) as api:
                resp = await api.history(peer, n)
            if resp.get("error"):
                self.append_chat(f"[error] history: {resp}")
                self.set_status("history failed")
                return
            items = resp.get("items", [])
            self.st.buffers[peer] = []
            for mid, (p, _) in list(self.st.msg_index.items()):
                if p == peer:
                    self.st.msg_index.pop(mid, None)
            for it in items:
                m = ChatMsg(
                    msg_id=it["msg_id"],
                    from_user=it["from_user"],
                    to_user=it["to_user"],
                    text=it["text"],
                    ts=int(it.get("created_at", _now_s())),
                    status="delivered",
                )
                self.st.buffer_append(peer, m)
            self.render_current()
            self.set_status("history loaded")
            return

        if cmd == "/sync":
            await self.do_sync()
            return

        if cmd == "/login":
            if len(parts) < 2:
                self.append_chat("usage: /login <username>")
                return
            user = parts[1].strip()
            # prompt password in terminal (no echo) - simple fallback:
            pw = await input_dialog(
                title="Login",
                text=f"Password for {user}:",
                password=True
            ).run_async()

            if pw is None:
                self.append_chat("[info] login cancelled")
                return

            await self.do_login(user, pw)
            await self.do_register(user, pw)
            return

        if cmd == "/register":
            if len(parts) < 2:
                self.append_chat("usage: /register <username>")
                return
            user = parts[1].strip()
            pw = await input_dialog(
                title="Register",
                text=f"Password for {user} (min6):",
                password=True
            ).run_async()

            if pw is None:
                self.append_chat("[info] register cancelled")
                return

            await self.do_register(user, pw)
            await self.do_login(user, pw)
            return

        self.append_chat(f"unknown command: {cmd}. try /help")

    async def run(self):
        await self.ensure_login()

        # If we have current peer saved, render it
        if self.st.current_peer:
            self.render_current()
            self.set_status(f"ready (peer={self.st.current_peer})")
        else:
            self.render_current()
            self.set_status("ready")

        # Start WS loop
        ws_task = asyncio.create_task(self.ws_connect_loop())

        try:
            await self.app.run_async()
        finally:
            self.st.stop_flag = True
            ws_task.cancel()
            with contextlib.suppress(asyncio.CancelledError):
                await ws_task
            self.persist_cfg()


# ----------------------------
# Main
# ----------------------------

import contextlib

async def main():
    st = State()

    # Load cfg for base_url early (so /base not required)
    cfg = load_cfg()
    if cfg.get("base_url"):
        st.base_url = cfg["base_url"]
        st.ws_base = base_to_ws(st.base_url)

    tui = TUI(st)
    await tui.run()

if __name__ == "__main__":
    try:
        asyncio.run(main())
    except KeyboardInterrupt:
        pass