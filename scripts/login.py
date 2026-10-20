#!/usr/bin/env python3
"""Interactive login tool for WorkBuddy CPA."""

import json
import os
import sys
import time
import urllib.parse
import urllib.request
import webbrowser
from pathlib import Path

def main():
    realm = sys.argv[1] if len(sys.argv) > 1 else "cn"
    if realm == "ai":
        base = "https://www.workbuddy.ai"
    elif realm == "copilot":
        base = "https://copilot.tencent.com"
    else:
        base = "https://www.workbuddy.cn"

    print(f"[*] Starting login flow for realm: {base} ...")

    # 1. Start auth state
    req = urllib.request.Request(
        f"{base}/v2/plugin/auth/state?platform=CLI",
        data=b"{}",
        headers={
            "User-Agent": "CLI/2.63.2 CodeBuddy/2.63.2",
            "Content-Type": "application/json",
            "Origin": base,
            "Referer": f"{base}/",
        },
    )

    try:
        with urllib.request.urlopen(req) as resp:
            data = json.loads(resp.read().decode("utf-8"))
    except Exception as e:
        print(f"[!] Failed to get auth state: {e}")
        return 1

    auth_data = data.get("data", {})
    state = auth_data.get("state")
    auth_url = auth_data.get("authUrl")

    if not state or not auth_url:
        print(f"[!] Unexpected response: {data}")
        return 1

    print("\n" + "=" * 60)
    print(f"请在浏览器中打开以下链接进行登录认证：\n\n{auth_url}\n")
    print("=" * 60 + "\n")

    try:
        webbrowser.open(auth_url)
    except Exception:
        pass

    print("[*] 正在等待浏览器中完成授权登录（按 Ctrl+C 取消）...")

    poll_url = f"{base}/v2/plugin/auth/token?state={state}"
    poll_req = urllib.request.Request(
        poll_url,
        headers={
            "User-Agent": "CLI/2.63.2 CodeBuddy/2.63.2",
            "Origin": base,
            "Referer": f"{base}/",
        },
    )

    token_info = None
    for _ in range(60):
        time.sleep(3)
        try:
            with urllib.request.urlopen(poll_req) as resp:
                res = json.loads(resp.read().decode("utf-8"))
                if res.get("code") == 0 and res.get("data", {}).get("accessToken"):
                    token_info = res["data"]
                    break
        except Exception:
            pass

    if not token_info:
        print("[!] 登录超时，未检测到授权，请重试。")
        return 1

    access_token = token_info.get("accessToken")
    refresh_token = token_info.get("refreshToken", "")
    expires_in = token_info.get("expiresIn", 7200)

    # Fetch account info
    acct_url = f"{base}/v2/plugin/login/account?state={state}"
    acct_req = urllib.request.Request(
        acct_url,
        headers={
            "User-Agent": "CLI/2.63.2 CodeBuddy/2.63.2",
            "Origin": base,
            "Referer": f"{base}/",
        },
    )
    account_info = {}
    try:
        with urllib.request.urlopen(acct_req) as resp:
            res = json.loads(resp.read().decode("utf-8"))
            if res.get("code") == 0:
                account_info = res.get("data", {})
    except Exception:
        pass

    auth_payload = {
        "auth": {
            "accessToken": access_token,
            "refreshToken": refresh_token,
            "domain": urllib.parse.urlparse(base).hostname,
            "expiresAt": int(time.time()) + expires_in,
        },
        "account": {
            "uid": account_info.get("uid", ""),
            "enterpriseId": account_info.get("enterpriseId", ""),
            "nickname": account_info.get("nickname", ""),
        },
        "baseURL": base,
    }

    # Save to both locations
    for target in [
        Path("workbuddy.json"),
        Path.home() / ".cli-proxy-api/workbuddy.json",
    ]:
        try:
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_text(json.dumps(auth_payload, indent=2, ensure_ascii=False) + "\n")
            os.chmod(target, 0o600)
            print(f"[+] 成功保存有效凭据至: {target}")
        except Exception as e:
            print(f"[!] 保存失败 {target}: {e}")

    print("\n[✔] 登录成功！凭据已更新至 Docker CPA 挂载目录。")
    return 0

if __name__ == "__main__":
    sys.exit(main())
