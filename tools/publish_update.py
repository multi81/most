"""publish_update.py — выпуск обновления «Моста» на сервер (2026-11-15, Вера).

  python publish_update.py <путь к most-X.Y.Z.exe> <версия> ["что нового"] [ssh-хост, по умолчанию wst]

Считает SHA-256, подписывает строку "most|<версия>|<sha256>" ключом Ed25519 (keys/most/chasti/update-sign.key,
секрет — вне git), кладёт на сервер /var/lib/most-admin/update/most.exe и latest.json. Программы на устройствах
(с 0.9.24 — сначала с GitHub Releases, потом с сервера) проверяют подпись открытым ключом, зашитым в них (update.go), — подменённую сборку не поставят.
"""
import base64, hashlib, json, os, subprocess, sys, tempfile

from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey

KEY = os.path.join(os.path.dirname(__file__), "..", "keys", "most", "chasti", "update-sign.key")


def main():
    exe, ver = sys.argv[1], sys.argv[2]
    notes = sys.argv[3] if len(sys.argv) > 3 else ""
    host = sys.argv[4] if len(sys.argv) > 4 else "wst"
    data = open(exe, "rb").read()
    sha = hashlib.sha256(data).hexdigest()
    key = Ed25519PrivateKey.from_private_bytes(base64.b64decode(open(KEY).read().strip()))
    sig = base64.b64encode(key.sign(f"most|{ver}|{sha}".encode())).decode()
    info = {"version": ver, "sha256": sha, "size": len(data), "sig": sig, "notes": notes}
    out = os.environ.get("MOST_LATEST_OUT")  # сохранить latest.json и для выпуска на GitHub
    if out:
        open(out, "w", encoding="utf-8").write(json.dumps(info, ensure_ascii=False))
    if host == "-":  # только GitHub, без сервера
        print(f"latest.json: {ver}, sha256 {sha[:16]}…")
        return
    with tempfile.TemporaryDirectory() as d:
        lj = os.path.join(d, "latest.json")
        open(lj, "w", encoding="utf-8").write(json.dumps(info, ensure_ascii=False))
        # сначала exe, потом latest.json — чтобы устройства не увидели новую версию раньше файла
        remote = os.environ.get("MOST_REMOTE_EXE")  # exe уже на сервере (собран там) — не гнать 37 МБ через канал
        if remote:
            got = subprocess.run(["ssh", host, f"cp {remote} /var/lib/most-admin/update/most.exe.new && "
                                  "sha256sum /var/lib/most-admin/update/most.exe.new"],
                                 check=True, capture_output=True, text=True).stdout.split()[0]
            if got != sha:
                sys.exit(f"на сервере другой файл: {got[:16]}… вместо {sha[:16]}…")
        else:
            subprocess.run(["scp", "-q", exe, f"{host}:/var/lib/most-admin/update/most.exe.new"], check=True)
        subprocess.run(["ssh", host, "mv /var/lib/most-admin/update/most.exe.new /var/lib/most-admin/update/most.exe"], check=True)
        subprocess.run(["scp", "-q", lj, f"{host}:/var/lib/most-admin/update/latest.json"], check=True)
    print(f"выпущено: {ver}, {len(data)} байт, sha256 {sha[:16]}…")


if __name__ == "__main__":
    main()
