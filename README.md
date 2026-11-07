# discordia-SERVICIO

Servicio de <Epica> de Discordia. Cubre las historias: <lista de historias>.

## Stack
- Lenguaje: Go
- Framework: Gin (github.com/gin-gonic/gin)
- BDD: PostgreSQL
- Infra extra: <completar>

## Setup local
```bash
cp .env.example .env
docker compose up -d db
make run
```

Para probar pagos, tarjetas de prueba y simuladores: [`docs/testing.md`](docs/testing.md).

## Variables de entorno
| Variable | Default | Descripcion |
|---|---|---|
| ENVIRONMENT | development | `development` o `production`. |
| HOST | 0.0.0.0 | IP donde escucha el servicio. `0.0.0.0` permite acceso desde otras redes/contenedores; `127.0.0.1` restringe a conexiones locales. |
| PORT | 8080 | Puerto donde escucha el servicio. La API REST y el WebSocket comparten `${HOST}:${PORT}`. |
| DATABASE_HOST | localhost | Host de PostgreSQL. |
| DATABASE_PORT | 5432 | Puerto de PostgreSQL. |
| DATABASE_NAME | - | Nombre de la base. En local, coincide con `POSTGRES_DB` de docker-compose.yml. |
| DATABASE_USER | - | Usuario de conexion. En local, coincide con `POSTGRES_USER` de docker-compose.yml. |
| DATABASE_PASSWORD | - | Password de conexion. En local, coincide con `POSTGRES_PASSWORD` de docker-compose.yml. |
| JWT_SECRET | - | Secreto para firmar/validar JWT. |
| RABBITMQ_URL | - | URL de conexion a RabbitMQ. |

`.env.example` solo trae la estructura (sin valores sensibles). Los valores reales van en tu propio `.env`, que nunca se sube (ya esta en `.gitignore`).

## Estructura de carpetas
```
go-template/
├── cmd/
│   └── api/
│       └── main.go          entrypoint: arma el router y levanta el server
├── internal/
│   ├── handler/              handlers HTTP (reciben el request, parsean, llaman a service)
│   ├── service/               logica de negocio
│   ├── repository/            acceso a datos (Postgres, etc.)
│   ├── model/                 structs de dominio / DTOs
│   ├── middleware/            middlewares de Gin (auth, logging, etc.)
│   └── events/                 publish/consume de eventos (RabbitMQ, etc.)
├── migrations/                migraciones de BDD (ej. golang-migrate)
├── .github/
│   └── workflows/
│       └── ci.yml            gofmt, build, vet y test en cada PR/push a main
├── .githooks/
│   └── pre-commit             hook de gofmt local
├── openapi.yaml               contrato de la API REST
├── docker-compose.yml         levanta el servicio + Postgres para desarrollo local
├── Dockerfile                 build multi-stage para la imagen de produccion
├── Makefile                   comandos run/test/build/migrate
└── .env.example                estructura de variables de entorno (sin valores sensibles)
```
`internal/` esta pensado para no exponer estos paquetes fuera del modulo (restriccion de Go). Las carpetas de `internal/` y `migrations/` vienen vacias (`.gitkeep`) porque el codigo de negocio todavia no existe en el template.

## Comandos (Makefile)
- `make run` - corre el servicio localmente (`go run ./cmd/api`).
- `make test` - corre los tests con cobertura (`go test ./... -cover`).
- `make build` - compila el binario en `bin/api`.
- `make migrate` - placeholder, todavia hay que definir la herramienta de migraciones.
- `make republish-premium` - escribe en el outbox de la base del `.env` un `billing.premium_updated` con el estado actual de cada usuario. Sirve para un consumer cuya cola no existia cuando salieron los eventos. Los publica el relay del billing que este corriendo contra esa base, y se puede repetir sin problema.

## Tests
```bash
make test
```
Cobertura minima: 70% (requisito del proyecto).

## Endpoints
Ver openapi.yaml.

## Eventos que publica
Todos van firmados con `BILLING_EVENTS_HMAC_SECRET`.
- `billing.premium_updated` - cuando cambia el Premium de un usuario
- `billing.server_boost_requested` - al reservar los puntos de un boost, y de nuevo cada 5 minutos mientras siga `pending`
- `billing.server_boost_cancelled` - a las 24 horas sin respuesta de `servers`, después de devolver los puntos
- `billing.server_boost_removed` - cuando el dueño retira un boost, sin devolución

## Eventos que consume
Cola `billing.servers_events`:
- `servers.server_boost_applied` - pasa el boost a `active`
- `servers.server_boost_rejected` - pasa el boost a `failed` y devuelve los puntos
- `servers.server_deleted` - pasa a `removed` los boosts activos del servidor, sin devolución

Los dos primeros se verifican con la firma antes de tocar nada. Solo una regla de negocio rechaza un boost: si falla la base, el evento se reencola y los puntos no se devuelven.

## Health checks
- `GET /livez` - el proceso está vivo. No consulta la base ni ningún servicio externo.
- `GET /readyz` - el servicio puede operar (pinguea la base; 503 si no responde). No comparte lógica con `/livez`.

## Logs

Cada línea que escribe el servicio es un objeto JSON por la salida estándar, con `time`, `level` (`debug`, `info`, `warning` o `error`), `service` (`billing`) y `message`. Es el mismo formato que usan `messaging`, `notifications` y `voice-sfu`.

- **Nivel configurable** con `LOG_LEVEL` (`debug`, `info`, `warning` o `error`; por defecto `info`). Un valor inválido hace fallar el arranque con un mensaje claro. Al arrancar escribe `service_started` con el nivel elegido.
- **Qué significa cada nivel:**
  - `error`: falla que impide ejecutar la funcionalidad (no se pudo guardar, la base no responde, un evento agotó los reintentos).
  - `warning`: condición anómala que no impide la funcionalidad básica (un evento descartado por inválido o por firma mala, una conexión a RabbitMQ que se reintenta, una configuración opcional que falta).
  - `info`: acción iniciada por una persona o por el sistema y trazable (evento procesado, barrido de vencimientos, pedido HTTP).
  - `debug`: contexto técnico para diagnosticar (eventos ignorados por viejos o repetidos, `/livez` y `/readyz` cuando responden bien).
- **Pedidos HTTP:** se registran como `http_request` con `method`, `route`, `status` y `duration_ms`. `route` es el **patrón** (`/v1/servers/:serverId`), nunca la ruta real ni la query string, porque ahí viajan ids, invitaciones y tokens. Un pedido sin ruta sale como `unmatched`. Un 5xx se registra como `error`.
- **Qué no se escribe:** el valor de campos llamados `password`, `token`, `ticket`, `jwt`, `authorization`, `cookie`, `secret`, `api_key`, `email` o `content` sale como `[FILTERED]`. Como red de seguridad, cualquier email o JWT que aparezca dentro de un texto (por ejemplo, en el mensaje de un error de la base) se reemplaza por `[EMAIL]` o `[JWT]`.
- **Pánicos:** se registran como `panic_recovered` con la ruta, el error y la traza, sin volcar los headers del pedido.
- El código vive en `internal/logging`. Las llamadas viejas al paquete `log` de Go también salen como JSON, porque `Setup` redirige el logger estándar.

## Deploy
Un push a `main`, si pasan los tests, corre tres jobs de `.github/workflows/ci.yml`:

1. `publish`: publica la imagen en `ghcr.io/discordia-grupo01/billing:<sha>`.
2. `migrate`: aplica las migraciones en produccion. Si fallan, no se despliega.
3. `deploy`: entra por SSH a la VM y corre `~/deploy.sh`, que levanta el contenedor en el
   puerto 8080, prueba `/readyz` y, si no responde, vuelve a la imagen anterior.

El puerto 8080 no esta abierto a internet: Kong llega por la IP de Tailscale de la VM. Las
variables del servicio estan en `~/billing.env` en la VM (ver `.env.example`), con
`ENVIRONMENT=production` y sin `BILLING_DEV_TOOLS`.

**Secrets de GitHub Actions:** `BILLING_VM_HOST`, `BILLING_VM_USER`, `BILLING_VM_SSH_KEY` y
`PROD_DATABASE_HOST`, `_PORT`, `_NAME`, `_USER`, `_PASSWORD`, `_SSLMODE`.

**Si cambia `deploy/deploy.sh`** hay que copiarlo a mano a la VM, porque el CI usa la copia
de `~/deploy.sh`:
```bash
scp deploy/deploy.sh ubuntu@<ip de la VM>:~/deploy.sh
```

## Formateo automatico
Este repo se autoformatea con gofmt en cada commit. Activalo una vez por clone:
```bash
git config core.hooksPath .githooks
```
Si usan VS Code con la extension oficial de Go, tambien formatea al guardar (ya configurado en .vscode/settings.json).

El CI corre `gofmt -l` como red de seguridad (falla si encuentra archivos sin formatear), por si alguien clono sin activar el hook.
