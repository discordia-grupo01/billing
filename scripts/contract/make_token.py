"""Firma un access token de prueba (HS256) como el que emite identify.

Uso: make_token.py <secret> <sub>
Imprime el JWT. Sin dependencias: solo la biblioteca estandar.
"""
import base64
import hashlib
import hmac
import json
import sys
import time


def b64(raw: bytes) -> str:
    return base64.urlsafe_b64encode(raw).rstrip(b"=").decode()


def main() -> None:
    secret, sub = sys.argv[1], sys.argv[2]
    header = b64(json.dumps({"alg": "HS256", "typ": "JWT"}, separators=(",", ":")).encode())
    now = int(time.time())
    # billing exige exp: una hora alcanza de sobra para una corrida.
    payload = b64(json.dumps({"sub": sub, "iat": now, "exp": now + 3600}, separators=(",", ":")).encode())
    signing_input = f"{header}.{payload}".encode()
    signature = b64(hmac.new(secret.encode(), signing_input, hashlib.sha256).digest())
    print(f"{header}.{payload}.{signature}")


if __name__ == "__main__":
    main()
