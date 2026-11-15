#!/usr/bin/env bash
# Valida que billing cumpla su openapi.yaml con Schemathesis.
# Levanta el servicio contra un Postgres y un RabbitMQ LOCALES y efimeros,
# nunca contra servicios remotos, y sin tocar Mercado Pago. Uso: make contract-test
# Las dependencias las levanta `make contract-deps-up` (o, en el CI, los
# servicios del job); las migraciones ya tienen que estar aplicadas.
set -euo pipefail

cd "$(dirname "$0")/.."

# Puertos de la tabla del plan de contratos (billing).
DB_HOST="${CONTRACT_DB_HOST:-localhost}"
DB_PORT="${CONTRACT_DB_PORT:-55503}"
DB_NAME="${CONTRACT_DB_NAME:-discordia_billing_contract}"
MQ_HOST="${CONTRACT_MQ_HOST:-localhost}"
MQ_PORT="${CONTRACT_MQ_PORT:-55513}"
APP_PORT="${CONTRACT_APP_PORT:-18083}"
MAX_EXAMPLES="${CONTRACT_MAX_EXAMPLES:-25}"
REPORT_DIR="${CONTRACT_REPORT_DIR:-schemathesis-report}"
PG_CONTAINER="${CONTRACT_PG_CONTAINER:-contract-billing-pg}"

# Valores de juguete: no son secretos reales.
JWT_SECRET_TEST="contract-test-secret"
MP_WEBHOOK_SECRET_TEST="contract-webhook-secret"
USER_ID="00000000-0000-4000-8000-0000000000b1"
PURCHASE_ID="00000000-0000-4000-8000-0000000000c1"
BOOST_ID="00000000-0000-4000-8000-0000000000d1"
BOOST_TO_REMOVE_ID="00000000-0000-4000-8000-0000000000d2"
SERVER_ID="00000000-0000-4000-8000-0000000000e1"
WEBHOOK_DATA_ID="contract-ping"
WEBHOOK_REQUEST_ID="contract-request"

# Guard: se rechaza todo host que no sea local.
for host in "$DB_HOST" "$MQ_HOST"; do
  case "$host" in
    localhost | 127.0.0.1 | ::1) ;;
    *)
      echo "contract-test: host no local '$host'; este script solo corre contra servicios locales." >&2
      exit 1
      ;;
  esac
done

PY="${PYTHON:-python}"
# Schemathesis imprime caracteres que la consola de Windows (cp1252) no puede escribir.
export PYTHONUTF8=1 PYTHONIOENCODING=utf-8

VENV=".contract-venv"
if [ -x "$VENV/bin/schemathesis" ]; then
  ST="$VENV/bin/schemathesis"
elif [ -x "$VENV/Scripts/schemathesis.exe" ]; then
  ST="$VENV/Scripts/schemathesis.exe"
elif command -v schemathesis >/dev/null 2>&1; then
  ST="schemathesis"
else
  "$PY" -m venv "$VENV"
  if [ -x "$VENV/bin/pip" ]; then PIP="$VENV/bin/pip"; else PIP="$VENV/Scripts/pip.exe"; fi
  "$PIP" install -q "schemathesis==4.33.4"
  if [ -x "$VENV/bin/schemathesis" ]; then ST="$VENV/bin/schemathesis"; else ST="$VENV/Scripts/schemathesis.exe"; fi
fi

TOKEN="$("$PY" scripts/contract/make_token.py "$JWT_SECRET_TEST" "$USER_ID")"
WEBHOOK_SIGNATURE="$("$PY" scripts/contract/make_signature.py "$MP_WEBHOOK_SECRET_TEST" "$WEBHOOK_DATA_ID" "$WEBHOOK_REQUEST_ID")"

# run_sql ejecuta SQL en la base efimera: con psql si esta instalado (el runner
# del CI lo trae) y, si no, dentro del contenedor de contract-deps-up.
run_sql() {
  if command -v psql >/dev/null 2>&1; then
    PGPASSWORD=discordia psql -q -v ON_ERROR_STOP=1 -h "$DB_HOST" -p "$DB_PORT" -U discordia -d "$DB_NAME" -c "$1"
  elif command -v docker >/dev/null 2>&1; then
    docker exec "$PG_CONTAINER" psql -q -v ON_ERROR_STOP=1 -U discordia -d "$DB_NAME" -c "$1"
  else
    echo "contract-test: hace falta psql o docker para sembrar la base." >&2
    exit 1
  fi
}

# Siembra el usuario de prueba con saldo, una compra pagada y dos boosts, para
# que los listados y las rutas por id tengan una rama de exito que recorrer.
run_sql "
INSERT INTO point_wallets (user_id, balance) VALUES ('$USER_ID', 1000000) ON CONFLICT (user_id) DO NOTHING;
INSERT INTO point_ledger (user_id, delta, reason, ref_id) VALUES ('$USER_ID', 1000000, 'purchase', 'contract-seed') ON CONFLICT DO NOTHING;
INSERT INTO point_purchases (id, user_id, package_id, points, amount_cents, status)
  VALUES ('$PURCHASE_ID', '$USER_ID', 'points_550', 550, 500000, 'paid') ON CONFLICT DO NOTHING;
INSERT INTO server_boosts (id, user_id, server_id, points, status)
  VALUES ('$BOOST_ID', '$USER_ID', '$SERVER_ID', 100, 'active'), ('$BOOST_TO_REMOVE_ID', '$USER_ID', '$SERVER_ID', 100, 'active')
  ON CONFLICT DO NOTHING;
"

LOG="$(mktemp)"
CONFIG="$(mktemp)"
# Ids reales para las rutas con id en el path (con ids inventados solo se llega
# al 404) y un aviso de webhook con firma valida y un topico que el servicio no
# maneja: responde 200 sin consultar a Mercado Pago. Con firmas al azar solo se
# llega al 401. Es un archivo temporal porque la firma lleva la hora.
cat >"$CONFIG" <<TOML
[[operations]]
include-name = "GET /v1/billing/purchases/{id}"
parameters = { id = "$PURCHASE_ID" }

[[operations]]
include-name = "GET /v1/billing/boosts/{id}"
parameters = { id = "$BOOST_ID" }

[[operations]]
include-name = "DELETE /v1/billing/boosts/{id}"
parameters = { id = "$BOOST_TO_REMOVE_ID" }

[[operations]]
include-name = "POST /webhooks/mercadopago"
parameters = { "data.id" = "$WEBHOOK_DATA_ID", type = "contract_noop", "x-request-id" = "$WEBHOOK_REQUEST_ID", "x-signature" = "$WEBHOOK_SIGNATURE" }
TOML

export ENVIRONMENT=development HOST=127.0.0.1 PORT="$APP_PORT"
export DATABASE_HOST="$DB_HOST" DATABASE_PORT="$DB_PORT" DATABASE_USER=discordia DATABASE_PASSWORD=discordia
export DATABASE_NAME="$DB_NAME" DATABASE_SSLMODE=disable
export JWT_SECRET="$JWT_SECRET_TEST"
export RABBITMQ_URL="amqp://guest:guest@$MQ_HOST:$MQ_PORT/"
export BILLING_EVENTS_HMAC_SECRET="contract-events-secret"
# Mercado Pago: valores de juguete. Ninguna operacion que se prueba llega al
# proveedor; ademas, el proxy muerto de abajo hace fallar cualquier salida.
export MP_ACCESS_TOKEN="TEST-contract" MP_WEBHOOK_SECRET="$MP_WEBHOOK_SECRET_TEST"
export CHECKOUT_RETURN_URL="http://localhost:3000"
export BILLING_DEV_TOOLS=false
unset MP_TEST_PAYER_EMAIL OTEL_EXPORTER_OTLP_ENDPOINT OTEL_EXPORTER_OTLP_HEADERS LOG_SHIP_URL LOG_SHIP_API_KEY

go build -o .contract-billing-api ./cmd/api
# El proxy muerto solo lo ve el servicio: Schemathesis y curl hablan con el por localhost.
HTTPS_PROXY="http://127.0.0.1:9" HTTP_PROXY="http://127.0.0.1:9" NO_PROXY="" ./.contract-billing-api >"$LOG" 2>&1 &
APP_PID=$!
cleanup() {
  kill "$APP_PID" 2>/dev/null || true
  # En Windows (Git Bash) kill no alcanza al proceso nativo: se lo mata por su PID.
  if command -v taskkill >/dev/null 2>&1; then
    WINPID="$(ps -p "$APP_PID" 2>/dev/null | awk 'NR==2 {print $4}')"
    if [ -n "${WINPID:-}" ]; then taskkill //F //PID "$WINPID" >/dev/null 2>&1 || true; fi
  fi
  rm -f .contract-billing-api .contract-billing-api.exe "$LOG" "$CONFIG"
}
trap cleanup EXIT

BASE="http://127.0.0.1:$APP_PORT"
ready=0
for _ in $(seq 1 60); do
  if curl -fsS "$BASE/readyz" >/dev/null 2>&1; then
    ready=1
    break
  fi
  if ! kill -0 "$APP_PID" 2>/dev/null; then
    echo "contract-test: el servicio no arrancó:" >&2
    cat "$LOG" >&2
    exit 1
  fi
  sleep 1
done
if [ "$ready" -ne 1 ]; then
  echo "contract-test: /readyz no respondió en 60 s:" >&2
  cat "$LOG" >&2
  exit 1
fi

# Schemathesis siempre manda un token valido; la rama 401 de las rutas
# autenticadas se comprueba a mano, igual que BILLING_DEV_TOOLS=false, que deja
# sin montar las herramientas de desarrollo.
expect_status() {
  local want="$1" got
  shift
  got="$(curl -s -o /dev/null -w '%{http_code}' "$@")"
  if [ "$got" != "$want" ]; then
    echo "contract-test: se esperaba $want y llegó $got para: $*" >&2
    exit 1
  fi
}
expect_status 401 "$BASE/v1/billing/points"
expect_status 401 -H "Authorization: Bearer no-es-un-jwt" "$BASE/v1/billing/premium"
expect_status 404 -X POST -H 'Content-Type: application/json' -d '{}' "$BASE/dev/billing/simulate-expiry"

# Operaciones excluidas, por llamar a Mercado Pago con datos validos:
#  - POST /v1/billing/points/checkout y POST /v1/billing/premium/checkout crean
#    la orden o la suscripcion en el proveedor (el SDK oficial no permite
#    cambiar su URL base). Quedan sin cubrir hasta tener un cliente falso.
# Checks desactivados (todos los demas quedan activos):
#  - negative_data_rejection: server_id= vacio en GET /boosts se toma como "no
#    enviado" (el contrato exige uuid y OpenAPI 3.0 no puede expresar el vacio
#    sin allowEmptyValue), y refresh acepta cualquier valor; rechazarlos
#    cambiaria lo que ve un cliente.
#  - positive_data_acceptance: hay reglas que el contrato no expresa, como que
#    una Idempotency-Key ya usada para otro servidor da 422, o que el validador
#    de uuid del cuerpo es mas estricto que format: uuid.
#  - unsupported_method: Gin responde 404 (no 405) a un metodo no declarado;
#    activar HandleMethodNotAllowed cambia el status que ve el cliente.
# El regex del filtro no empieza con "/" para que Git Bash en Windows no lo
# convierta en una ruta de disco.
"$ST" --config-file "$CONFIG" run openapi.yaml \
  --url "$BASE" \
  -H "Authorization: Bearer $TOKEN" \
  --exclude-path-regex '.*/(points|premium)/checkout$' \
  --max-examples "$MAX_EXAMPLES" \
  --seed 1 \
  --checks all \
  --exclude-checks negative_data_rejection,positive_data_acceptance,unsupported_method \
  --generation-database none \
  --report junit,vcr --report-dir "$REPORT_DIR"
