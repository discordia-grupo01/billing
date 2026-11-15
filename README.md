# billing

Servicio de pagos de Discordia.

## Tabla de contenidos

- [Introducción](#introducción)
- [Pre-requisitos](#pre-requisitos)
- [Cómo correr](#cómo-correr)
- [Variables de entorno](#variables-de-entorno)
- [Comandos Docker](#comandos-docker)
- [Tests](#tests)
- [Contrato de la API y tests de contrato](#contrato-de-la-api-y-tests-de-contrato)
- [Más documentación](#más-documentación)

## Introducción

`billing` es el servicio que maneja la plata de Discordia. Se divide en tres partes:

- **Premium:** suscribirse al plan Premium, ver el estado de la suscripción y cancelarla.
- **Puntos:** comprar paquetes de puntos y llevar el saldo y los movimientos de cada usuario.
- **Boosts:** gastar puntos para potenciar un servidor. Es una saga con `servers`: `billing` reserva los puntos, `servers` aplica o rechaza el boost, y si lo rechaza o no responde en 24 horas, `billing` devuelve los puntos.

Está hecho en Go con Gin y guarda todo en su propia base PostgreSQL. Los cobros los hace Mercado Pago, que avisa el resultado por un webhook. Ese webhook es la única ruta pública del servicio, porque Mercado Pago no manda JWT: se protege verificando la firma del pedido.

Cuando cambia el Premium de un usuario, `billing` publica un evento firmado (HMAC) para que `identify-service` y `messaging` lo repliquen. Antes de publicar anota el evento en una tabla de su base dentro de la misma transacción, así que si RabbitMQ se cae el evento no se pierde: solo se retrasa.

El gateway (Kong) deja pasar los pedidos y este servicio valida el JWT con `JWT_SECRET`. La API REST está descripta en [`openapi.yaml`](openapi.yaml).

## Pre-requisitos

- **Docker** y **Docker Compose** v2.24 o más nuevo.
- **RabbitMQ**, que se levanta con el repositorio [`rabbit-mq`](https://github.com/discordia-grupo01/rabbit-mq). El servicio no arranca sin él.
- **Credenciales de prueba de Mercado Pago** (sandbox), solo si querés probar un pago de verdad. Está explicado en [`docs/testing.md`](docs/testing.md). Sin ellas, el servicio arranca igual y se pueden consultar los paquetes de puntos y el saldo, pero no crear cobros.
- **Go 1.25** y **make**, solo si querés correr el servicio con Go en tu máquina.

## Cómo correr

### Con Docker Compose

Primero levantá RabbitMQ desde el repositorio `rabbit-mq`:

```bash
cd ../rabbit-mq
docker compose up -d
```

Después, en este repositorio, levantá el servicio con su base PostgreSQL:

```bash
docker compose up --build
```

Antes de arrancar el servicio, un contenedor `migrate` crea las tablas y termina. Cuando todo esté arriba, comprobá que el servicio esté vivo:

```bash
curl http://localhost:8080/readyz
```

El servicio se conecta a RabbitMQ en `host.docker.internal:5672`, que desde un contenedor es tu máquina. Si lo levantaste en otro lado, completá `RABBITMQ_URL_DOCKER` en el `.env`.

Para probar pagos, copiá el archivo de ejemplo y completá tus credenciales de sandbox de Mercado Pago:

```bash
cp .env.example .env
```

Este compose levanta solo lo que es de este servicio. Para el sistema completo (con el gateway y los demás servicios) usá el compose del repositorio `infrastructure`.

### Para desarrollar

Si vas a tocar el código, te conviene correr el servicio con Go en tu máquina y dejar en un contenedor solo la base:

```bash
cp .env.example .env
docker compose up -d db
make migrate
make run
```

Con el `.env` de ejemplo, el servicio se conecta a la base que acabás de levantar y a RabbitMQ en `localhost`. En este modo `BILLING_DEV_TOOLS=true` habilita dos rutas de ayuda (`/dev/billing/simulate-charge` y `simulate-expiry`) para demostrar renovaciones. No arranca con `ENVIRONMENT=production`.

## Variables de entorno

Se leen del entorno o de un archivo `.env`. El archivo [`.env.example`](.env.example) trae valores listos para correr en local.

| Variable | Default | Descripción |
|---|---|---|
| `ENVIRONMENT` | `development` | `development` o `production`. |
| `HOST` | `0.0.0.0` | IP donde escucha el servicio. |
| `PORT` | `8080` | Puerto donde escucha el servicio. |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warning` o `error`. |
| `DATABASE_HOST` | obligatoria | Host de PostgreSQL. |
| `DATABASE_PORT` | `5432` | Puerto de PostgreSQL. |
| `DATABASE_NAME` | obligatoria | Nombre de la base. |
| `DATABASE_USER` | obligatoria | Usuario de la base. |
| `DATABASE_PASSWORD` | obligatoria | Contraseña de la base. |
| `DATABASE_SSLMODE` | `disable` | `disable` en local, `require` con Supabase. |
| `JWT_SECRET` | obligatoria | Secreto con el que se valida el JWT. Es un secreto. |
| `RABBITMQ_URL` | obligatoria | Conexión a RabbitMQ. |
| `BILLING_EVENTS_HMAC_SECRET` | obligatoria | Secreto con el que se firma `billing.premium_updated`. Tiene que ser el mismo en `identify-service` y `messaging`. |
| `MP_ACCESS_TOKEN` | obligatoria | Token de acceso de Mercado Pago (sandbox o producción). Es un secreto. |
| `MP_WEBHOOK_SECRET` | obligatoria | Clave con la que se valida la firma del webhook de Mercado Pago. Es un secreto. |
| `CHECKOUT_RETURN_URL` | obligatoria | URL pública de la web, a la que Mercado Pago devuelve al usuario después de pagar. |
| `MP_TEST_PAYER_EMAIL` | vacío | Email del comprador de prueba. Solo en sandbox. |
| `BILLING_DEV_TOOLS` | `false` | Habilita las rutas de simulación. Solo en local. |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | vacío | Backend OpenTelemetry (por ejemplo New Relic). Vacío = sin trazas ni métricas. |
| `OTEL_EXPORTER_OTLP_HEADERS` | vacío | Credenciales del backend. Es un secreto. |
| `LOG_SHIP_URL` | vacío | Si se completa, también manda los logs a New Relic. |

Variables que usa solo `docker-compose.yml`:

| Variable | Default | Descripción |
|---|---|---|
| `API_PORT` | `8080` | Puerto del servicio en tu máquina. |
| `DATABASE_PORT` | `5432` | Puerto de PostgreSQL en tu máquina. |
| `RABBITMQ_URL_DOCKER` | `amqp://guest:guest@host.docker.internal:5672/` | Conexión a RabbitMQ para el servicio que corre dentro del compose. |

## Comandos Docker

Todos desde la raíz del repositorio.

| Qué querés hacer | Comando |
|---|---|
| Levantar todo | `docker compose up --build` |
| Levantar todo en segundo plano | `docker compose up -d --build` |
| Levantar solo la base | `docker compose up -d db` |
| Ver los logs del servicio | `docker compose logs -f api` |
| Ver qué está corriendo | `docker compose ps` |
| Reconstruir solo el servicio | `docker compose up -d --build api` |
| Abrir una consola de la base | `docker compose exec db psql -U discordia -d discordia_servicio` |
| Bajar todo, conservando los datos | `docker compose down` |
| Bajar todo y borrar los datos | `docker compose down -v` |

Las migraciones nuevas se aplican solas cada vez que levantás el compose.

Para construir y correr la imagen a mano, contra una base que ya tengas:

```bash
docker build -t billing .
docker run --rm -p 8080:8080 --env-file .env billing
```

Dentro de un contenedor, `DATABASE_HOST` y `RABBITMQ_URL` no pueden apuntar a `localhost`: tienen que usar el nombre del contenedor o `host.docker.internal`.

## Tests

Los tests unitarios no necesitan nada más:

```bash
make test
```

Para correr también los de integración, `make test-integration` levanta por su cuenta un PostgreSQL y un RabbitMQ descartables (en los puertos 55432 y 55672), corre toda la suite y los apaga. Nunca toca la base ni el broker de tu `.env`:

```bash
make test-integration
```

Probar un pago de punta a punta, con tarjetas de prueba y los simuladores, está explicado en [`docs/testing.md`](docs/testing.md).

El repositorio se formatea con gofmt. Activá el hook una vez por clon:

```bash
git config core.hooksPath .githooks
```

## Contrato de la API y tests de contrato

`openapi.yaml` se genera desde las anotaciones `swag` del código (`internal/handler` y `cmd/api/main.go`); no se edita a mano. Para regenerarlo y validarlo:

```bash
make openapi
```

El CI lo regenera en cada push y commitea el cambio si hubo alguno.

[Schemathesis](https://schemathesis.readthedocs.io/) (versión fija 4.33.4) prueba que el servicio real cumpla ese contrato: sin 5xx, con respuestas y cuerpos declarados. Hace falta Docker, Python 3 y Go; el script crea su propio entorno virtual en `.contract-venv/`:

```bash
make contract-deps-up   # Postgres (55503) y RabbitMQ (55513) descartables, y las migraciones
make contract-test      # levanta el servicio, siembra datos de prueba, corre Schemathesis y lo apaga
make contract-deps-down
```

Reglas de seguridad: el script se niega a correr si la base o RabbitMQ no son locales, usa secretos de juguete y no llega nunca a Mercado Pago (el servicio arranca con un proxy muerto y un token de prueba). El job `contract` del CI hace lo mismo con los servicios del job y no usa `secrets.*`.

Qué no cubre, y por qué:

- `POST /v1/billing/points/checkout` y `POST /v1/billing/premium/checkout` quedan excluidas: con datos válidos crean la orden o la suscripción en Mercado Pago, y su SDK oficial no permite cambiar la URL base.
- `POST /v1/billing/premium/cancel` solo ve el 409 (sin suscripción): el 200 exige una suscripción viva y llamar a Mercado Pago.
- Checks desactivados, con su motivo en `scripts/contract-test.sh`: `negative_data_rejection` (`server_id=` vacío y `refresh` aceptan cualquier valor), `positive_data_acceptance` (reglas que el contrato no expresa, como una `Idempotency-Key` ya usada) y `unsupported_method` (Gin responde 404 y no 405).
- Las herramientas de desarrollo `/dev/billing/simulate-*` no forman parte del contrato.

## Más documentación

En este repositorio:

- [`docs/historias-de-usuario.md`](docs/historias-de-usuario.md): las historias del catálogo que resuelve este servicio, con sus rutas.
- [`docs/testing.md`](docs/testing.md): cómo probar pagos en sandbox, con tarjetas de prueba y simuladores.
- [`docs/mutation-testing`](docs/mutation-testing/README.md): resultados del mutation testing de la lógica de pagos.
- [`openapi.yaml`](openapi.yaml): contrato de la API REST.

En el repositorio [`docs`](https://github.com/discordia-grupo01/docs):

- [`asyncapi`](https://github.com/discordia-grupo01/docs/tree/main/asyncapi): los eventos que este servicio publica y consume.
