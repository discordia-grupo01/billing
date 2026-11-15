"""Firma el x-signature de un aviso de Mercado Pago como lo hace el proveedor.

Uso: make_signature.py <secret> <data.id> <x-request-id>
Imprime `ts=<milisegundos>,v1=<hmac>`. Sin dependencias: solo la biblioteca estandar.
"""
import hashlib
import hmac
import sys
import time


def main() -> None:
    secret, data_id, request_id = sys.argv[1], sys.argv[2], sys.argv[3]
    ts = str(int(time.time() * 1000))
    manifest = f"id:{data_id.lower()};request-id:{request_id};ts:{ts};"
    digest = hmac.new(secret.encode(), manifest.encode(), hashlib.sha256).hexdigest()
    print(f"ts={ts},v1={digest}")


if __name__ == "__main__":
    main()
