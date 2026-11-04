# Cómo probar billing

Hay dos formas de probar billing:

- **[En producción](#probar-en-producción):** no hay que instalar nada. Sirve para ver los pagos de punta a punta, con webhooks reales.
- **[En local](#probar-en-local):** para desarrollar. Tiene los simuladores de renovación, pero no recibe webhooks de Mercado Pago.

Todo corre en **modo sandbox**: se usan credenciales de prueba y **no se cobra plata real**. Nunca uses una tarjeta real ni tu cuenta personal de Mercado Pago.

## Pagar en sandbox

Vale para las dos formas.

1. Abrí el `checkout_url` en una **ventana privada**. Si tenés sesión de Mercado Pago iniciada, te abre tu cuenta personal y el pago falla con *"Una de las partes con la que intentás hacer el pago es de prueba"*.
2. Elegí **Ingresar con mi cuenta** y entrá con el **comprador de prueba**: en "DNI, email o teléfono" va su usuario `TESTUSER…`, no un email. Usuario y contraseña se piden por privado a quien administra la app de Mercado Pago. Si pide un código de 6 dígitos, está en el panel de Mercado Pago, en Cuentas de prueba.
3. Pagá con una **tarjeta de prueba nueva** (Agregar tarjeta). Con el dinero disponible se aprueba siempre, y una tarjeta guardada usa el titular con el que se cargó.

### Tarjetas de prueba (Argentina)

| Tarjeta | Número |
|---|---|
| Visa | `4509 9535 6623 3704` |
| Mastercard | `5031 7557 3453 0604` |

Vencimiento `11/30`, CVV `123`, DNI `12345678`. **El nombre del titular decide el resultado:**

| Titular | Resultado | Sirve para probar |
|---|---|---|
| `APRO` | Aprobado | Compra y suscripción exitosas |
| `OTHE` | Rechazado | Pago rechazado |
| `FUND` | Fondos insuficientes | Pago rechazado |
| `CONT` | Pendiente | Pago en proceso |

### Qué tiene que pasar

| Prueba | Resultado esperado |
|---|---|
| Puntos con `APRO` | Compra `paid` / `accredited` y saldo sumado |
| Puntos con `OTHE` | Compra `failed` / `rejected_by_issuer` y sin puntos. La order sigue abierta: si se paga con otra tarjeta, se acredita |
| Premium con `APRO` | `state: active` y `premium_until` un mes adelante |
| Premium con `FUND` | `state: none` y `mp_status: cancelled`. Mercado Pago cancela la suscripción y no avisa por webhook: se ve con `?refresh=1` |
| Cancelar Premium | `state: active_canceled`, `active: true` y `renews: false`, hasta la fecha paga |

Después de pagar, Mercado Pago redirige a `<web-client>/billing/return`.

## Probar en producción

No hace falta ninguna variable. Solo una cuenta de Discordia y el comprador de prueba.

**1. Conseguir el token.** Dura 15 minutos.

Con cuenta de email y contraseña:

```bash
G=https://discordia-g1.duckdns.org
read -p "email: " EMAIL; read -s -p "contraseña: " PASS; echo
TOKEN=$(python3 -c 'import json,sys; print(json.dumps({"email":sys.argv[1],"password":sys.argv[2]}))' "$EMAIL" "$PASS" \
  | curl -s -X POST -H "content-type: application/json" -d @- $G/v1/login \
  | python3 -c 'import json,sys; print(json.load(sys.stdin)["token"])')
unset PASS
```

Si entrás con Google o tenés 2FA, ese login no sirve. Iniciá sesión en https://discordia-g1.vercel.app, abrí **F12** → **Almacenamiento** (Application en Chrome) → **Cookies**, copiá el valor de `discordia_session` y corré esto. La cookie es tan sensible como una contraseña: no la compartas.

```bash
G=https://discordia-g1.duckdns.org
read -s -p "cookie: " COOKIE; echo
TOKEN=$(COOKIE="$COOKIE" python3 -c '
import json,os,urllib.parse
s=json.loads(urllib.parse.unquote(os.environ["COOKIE"]))
print(s.get("token") or s.get("accessToken") or "")')
unset COOKIE
```

**2. Comprar puntos.** Paquetes: `points_100`, `points_550` y `points_1200`.

```bash
curl -s -H "Authorization: Bearer $TOKEN" $G/v1/billing/points/packages | python3 -m json.tool

curl -s -X POST -H "Authorization: Bearer $TOKEN" -d '{"package_id":"points_100"}' \
  $G/v1/billing/points/checkout | python3 -c 'import json,sys; print(json.load(sys.stdin)["checkout_url"])'

# Después de pagar: saldo e historial
curl -s -H "Authorization: Bearer $TOKEN" $G/v1/billing/points | python3 -m json.tool
```

**3. Premium.** Un usuario con una suscripción viva recibe `409 ALREADY_SUBSCRIBED`.

```bash
curl -s -X POST -H "Authorization: Bearer $TOKEN" $G/v1/billing/premium/checkout \
  | python3 -c 'import json,sys; print(json.load(sys.stdin)["checkout_url"])'

# Después de autorizar
curl -s -H "Authorization: Bearer $TOKEN" "$G/v1/billing/premium?refresh=1" | python3 -m json.tool

# Cancelar
curl -s -X POST -H "Authorization: Bearer $TOKEN" $G/v1/billing/premium/cancel | python3 -m json.tool
```

Los simuladores no existen en producción. Los datos de estas pruebas quedan en la base de producción.

## Probar en local

> **Nunca uses la base de producción en local.** Dos billing sobre la misma base se reparten los eventos del outbox, y los que publica el local no les llegan a los demás servicios.

**1. Levantar la base y RabbitMQ:**

```bash
docker compose up -d db
docker run -d --name rabbit -p 5672:5672 -p 15672:15672 rabbitmq:3-management-alpine
```

**2. Crear `.env`.** Va sin comillas: el Makefile no las saca y quedarían dentro del valor.

```
ENVIRONMENT=development
HOST=0.0.0.0
PORT=8080

DATABASE_HOST=localhost
DATABASE_PORT=5432
DATABASE_NAME=discordia_servicio
DATABASE_USER=discordia
DATABASE_PASSWORD=discordia
DATABASE_SSLMODE=disable

JWT_SECRET=dev-secret-change-me
RABBITMQ_URL=amqp://guest:guest@localhost:5672/
BILLING_EVENTS_HMAC_SECRET=dev-billing-events-secret

MP_ACCESS_TOKEN=
MP_WEBHOOK_SECRET=cualquier-valor
MP_TEST_PAYER_EMAIL=
CHECKOUT_RETURN_URL=https://example.com
BILLING_DEV_TOOLS=true
```

`MP_ACCESS_TOKEN` es el token de prueba y `MP_TEST_PAYER_EMAIL` es el email del comprador de prueba: `test_user_<números de su usuario TESTUSER>@testuser.com`, no su User ID. Pedilos por privado. **No los subas al repo ni los pegues en un grupo.**

**3. Migrar y arrancar.** `make run` carga el `.env`; `go run` solo no lo lee.

```bash
make migrate
make run
```

**4. Generar un token**, en otra terminal. Cada corrida crea un usuario nuevo, y el token dura una hora:

```bash
G=localhost:8080
TOKEN=$(python3 -c '
import base64,hashlib,hmac,json,time,uuid
b=lambda d: base64.urlsafe_b64encode(json.dumps(d).encode()).rstrip(b"=")
h=b({"alg":"HS256","typ":"JWT"}); p=b({"sub":str(uuid.uuid4()),"exp":int(time.time())+3600})
s=base64.urlsafe_b64encode(hmac.new(b"dev-secret-change-me",h+b"."+p,hashlib.sha256).digest()).rstrip(b"=")
print((h+b"."+p+b"."+s).decode())')
USER_ID=$(python3 -c "import base64,json,sys; p=sys.argv[1].split('.')[1]; print(json.loads(base64.urlsafe_b64decode(p+'=='))['sub'])" "$TOKEN")
```

Después se usan los mismos curls de [producción](#probar-en-producción).

**Sin webhooks:** Mercado Pago avisa a una sola URL, que es la de producción. En local, los pagos se detectan así:

- **Puntos:** el job de conciliación toma compras con más de 10 minutos y corre cada 15. También corre al arrancar, así que se puede reiniciar billing pasados los 10 minutos.
- **Premium:** al instante con `GET /v1/billing/premium?refresh=1`.

### Simular renovaciones

Mercado Pago no deja adelantar el tiempo. Con `BILLING_DEV_TOOLS=true` se puede simular, usando un usuario con una suscripción autorizada:

```bash
# Renovación aprobada: Premium suma un mes
curl -s -X POST $G/dev/billing/simulate-charge -d "{\"user_id\":\"$USER_ID\",\"outcome\":\"approved\"}" | python3 -m json.tool

# Renovación rechazada: no cambia nada
curl -s -X POST $G/dev/billing/simulate-charge -d "{\"user_id\":\"$USER_ID\",\"outcome\":\"rejected\"}" | python3 -m json.tool

# Llega la fecha sin cobro: state expired
curl -s -X POST $G/dev/billing/simulate-expiry -d "{\"user_id\":\"$USER_ID\"}" | python3 -m json.tool
```

### Ver los eventos

RabbitMQ descarta lo que llega sin una cola. Creala **antes** de pagar:

```bash
curl -u guest:guest -X PUT -H "content-type: application/json" \
  localhost:15672/api/exchanges/%2F/discordia.events -d '{"type":"topic","durable":true}'
curl -u guest:guest -X PUT -H "content-type: application/json" \
  localhost:15672/api/queues/%2F/prueba -d '{"durable":true}'
curl -u guest:guest -X POST -H "content-type: application/json" \
  localhost:15672/api/bindings/%2F/e/discordia.events/q/prueba -d '{"routing_key":"billing.#"}'
```

Se ven en http://localhost:15672 (`guest` / `guest`), en la cola `prueba`, con **Get messages**.

## Si algo falla

| Síntoma | Causa |
|---|---|
| `missing required environment variables` | Se corrió `go run`. Usá `make run` |
| `INTERNAL_ERROR` | El motivo está en el log de billing, en la línea `request failed` |
| `Invalid value for payer_email` | `MP_TEST_PAYER_EMAIL` tiene comillas |
| `User bad request` al crear Premium | El email no es del comprador de prueba: usa el User ID en lugar de los números de `TESTUSER…` |
| *"Una de las partes … es de prueba"* | Se pagó con una cuenta real. Usá ventana privada y el comprador de prueba |
| El pago se aprueba aunque querías un rechazo | Se usó dinero disponible o una tarjeta guardada con titular `APRO`. Cargá una nueva con otro titular |
| `UNAUTHORIZED` | Venció el token |
| `ALREADY_SUBSCRIBED` | Ese usuario ya tiene una suscripción viva. Cancelala o usá otro usuario |
| 404 al volver de Mercado Pago | Falta la pantalla `/billing/return` en el web-client. El pago se procesa igual |
