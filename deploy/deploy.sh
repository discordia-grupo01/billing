#!/bin/bash
# Deploy de billing en su VM de Oracle. Se instala en ~/deploy.sh y lo llama el
# job "deploy" del CI con la imagen del commit. Las variables de entorno viven
# en ~/billing.env en la VM: nunca se versionan.
set -euo pipefail

IMAGE="${1:-}"
if ! [[ "$IMAGE" =~ ^ghcr\.io/discordia-grupo01/billing:[0-9a-f]{40}$ ]]; then
  echo "imagen invalida: $IMAGE" >&2
  exit 2
fi

run() {
  docker run -d --name billing --restart unless-stopped \
    --env-file "$HOME/billing.env" -p 8080:8080 "$1" >/dev/null
}

# ID de la imagen que esta corriendo hoy, para poder volver atras.
PREV_ID=$(docker inspect -f '{{.Image}}' billing 2>/dev/null || true)

docker pull "$IMAGE"
docker rm -f billing >/dev/null 2>&1 || true
run "$IMAGE"

# Smoke test: /readyz confirma que el servicio y la base responden.
for _ in $(seq 1 20); do
  if curl -fsS -m 3 http://localhost:8080/readyz >/dev/null 2>&1; then
    echo "deploy OK: $IMAGE"
    # La VM tiene poco disco de sobra para imagenes viejas.
    docker image prune -af --filter "until=168h" >/dev/null 2>&1 || true
    exit 0
  fi
  sleep 3
done

echo "ERROR: /readyz no respondio tras el deploy de $IMAGE" >&2
docker logs --tail 30 billing 2>&1 \
  | sed -E 's#(amqps?://)[^@ ]*@#\1***@#g; s#(password=)[^ ]*#\1***#Ig' >&2 || true
docker rm -f billing >/dev/null 2>&1 || true
if [ -n "$PREV_ID" ]; then
  echo "rollback a la imagen anterior ($PREV_ID)" >&2
  run "$PREV_ID"
fi
exit 1
