#!/usr/bin/env python3
"""Import WorkBuddy AI.app desktop credentials into a workbuddy.json draft.

Reads the app's Chromium cookie database (read-only) and fills accessToken /
refreshToken / domain when obvious session cookies exist. Chromium-encrypted
values (v10+ prefix) cannot be decrypted here; candidate cookie names are then
printed (values masked) so the mapping can be done by hand.

Cookie values are credentials: the draft file is gitignored; do not commit it.
"""

from __future__ import annotations

import argparse
import json
import os
import shutil
import sqlite3
import sys
import tempfile
from pathlib import Path

HOME = Path.home()
APP_SUPPORT = HOME / "Library/Application Support"

CANDIDATE_DBS = [
    HOME / ".workbuddy/app/session/Cookies",
    HOME / ".workbuddy-ai/app/session/Cookies",
    APP_SUPPORT / "WorkBuddy AI/Cookies",
    APP_SUPPORT / "WorkBuddy AI/Partitions/workbuddy/Cookies",
    APP_SUPPORT / "WorkBuddy/Cookies",
    APP_SUPPORT / "workbuddy/Cookies",
]
TOKEN_NAMES = {
    "access_token": "accessToken",
    "accessToken": "accessToken",
    "refresh_token": "refreshToken",
    "refreshToken": "refreshToken",
}


def load_cookies(db: Path) -> list[tuple[str, str, str]]:
    # Try immutable read-only first
    uri = f"file:{db.resolve()}?mode=ro&immutable=1"
    try:
        con = sqlite3.connect(uri, uri=True)
        cur = con.cursor()
        cur.execute("SELECT host_key, name, value FROM cookies WHERE value != ''")
        rows = list(cur.fetchall())
        con.close()
        return rows
    except sqlite3.OperationalError:
        # Fall back to copying database and WAL to /tmp to avoid lock contention
        pass

    with tempfile.TemporaryDirectory() as tmpdir:
        tmp_db = Path(tmpdir) / "Cookies.db"
        shutil.copy2(db, tmp_db)
        for suffix in ("-wal", "-journal", "-shm"):
            sidecar = db.parent / (db.name + suffix)
            if sidecar.exists():
                try:
                    shutil.copy2(sidecar, Path(tmpdir) / (tmp_db.name + suffix))
                except Exception:
                    pass
        con = sqlite3.connect(f"file:{tmp_db}?mode=ro", uri=True)
        try:
            cur = con.cursor()
            cur.execute("SELECT host_key, name, value FROM cookies WHERE value != ''")
            return list(cur.fetchall())
        finally:
            con.close()

def looks_encrypted(value: str) -> bool:
    return value.startswith(("v10", "v11", "v12"))


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--db", type=Path, default=None,
                        help="cookie database path (default: probe known WorkBuddy AI.app paths)")
    parser.add_argument("-o", "--output", type=Path, default=Path("workbuddy.json"))
    args = parser.parse_args()

    db = args.db
    if db is None:
        for cand in CANDIDATE_DBS:
            if cand.exists():
                db = cand
                break
    if db is None or not db.exists():
        print("cookie db not found; tried:", file=sys.stderr)
        for cand in CANDIDATE_DBS:
            print(f"  {cand}", file=sys.stderr)
        print("pass --db explicitly (WorkBuddy AI.app keeps its profile under "
              "Library/Application Support/<app>/...Cookies)", file=sys.stderr)
        return 1

    rows = [(h, n, v) for (h, n, v) in load_cookies(db)
            if "workbuddy" in h or "codebuddy" in h]
    if not rows:
        print(f"no workbuddy/codebuddy cookies in {db}; login to WorkBuddy AI.app first",
              file=sys.stderr)
        return 2

    fields: dict[str, str] = {}
    domain = ""
    encrypted: list[str] = []
    candidates: list[str] = []
    for host, name, value in rows:
        if looks_encrypted(value):
            encrypted.append(f"{name}@{host}")
            continue
        field = TOKEN_NAMES.get(name)
        if field and not fields.get(field):
            fields[field] = value
            domain = domain or host.lstrip(".")
        elif name == "session" and "workbuddy" in host:
            # WorkBuddy desktop webview session cookie carries auth
            if not fields.get("accessToken"):
                fields["accessToken"] = value
                domain = domain or host.lstrip(".")
        else:
            candidates.append(f"{name}@{host}")
    if not fields.get("accessToken") and not fields.get("refreshToken"):
        print("no plaintext token cookies found; values are likely Chromium-encrypted "
              "or use different names. Candidate cookie names (values masked):",
              file=sys.stderr)
        for item in encrypted + candidates:
            print(f"  {item}", file=sys.stderr)
        print("map the right ones into workbuddy.json by hand (see workbuddy.json.example)",
              file=sys.stderr)
        return 3

    # Deep merge with existing destination file if present
    existing: dict = {}
    if args.output.exists():
        try:
            existing = json.loads(args.output.read_text())
        except Exception:
            existing = {}

    auth_block = existing.get("auth", {})
    auth_block["accessToken"] = fields.get("accessToken") or auth_block.get("accessToken", "")
    auth_block["refreshToken"] = fields.get("refreshToken") or auth_block.get("refreshToken", "")
    auth_block["domain"] = domain or auth_block.get("domain", "www.workbuddy.cn")
    existing["auth"] = auth_block
    if "account" not in existing:
        existing["account"] = {}

    args.output.write_text(json.dumps(existing, ensure_ascii=False, indent=2) + "\n")
    try:
        os.chmod(args.output, 0o600)
    except Exception:
        pass
    print(f"wrote draft to {args.output} (domain={auth_block['domain']}); "
          "verify before use and never commit it")
    return 0


if __name__ == "__main__":
    sys.exit(main())
