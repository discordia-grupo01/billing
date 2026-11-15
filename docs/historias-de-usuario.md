# Historias de usuario

Las historias del catálogo de la cátedra que resuelve este servicio, con su número, y las que resuelve junto con otros servicios. Las rutas completas con sus parámetros y respuestas están en [`openapi.yaml`](../openapi.yaml).

## Optativas

| # | Historia | Puntos | Rutas |
|---|---|---|---|
| 76 | Suscribirse a Plan Premium | 13 | `POST /v1/billing/premium/checkout`, `GET /v1/billing/premium`, `POST /webhooks/mercadopago` |
| 77 | Cancelar suscripción Premium | 3 | `POST /v1/billing/premium/cancel` |
| 78 | Comprar puntos | 8 | `GET /v1/billing/points/packages`, `POST /v1/billing/points/checkout`, `GET /v1/billing/points`, `GET /v1/billing/purchases/{id}` |
| 79 | Usar puntos para boostear un servidor | 13 | `POST /v1/billing/boosts`, `GET /v1/billing/boosts`, `GET /v1/billing/boosts/servers`, `GET /v1/billing/boosts/{id}`, `DELETE /v1/billing/boosts/{id}` |

## Historias en las que participa

Estas historias las completan otros servicios. Acá está la parte que le toca a `billing`:

| # | Historia | Qué hace este servicio |
|---|---|---|
| 47 | Adjuntar archivos en mensajes | Publica el estado de Premium (evento firmado) para que `messaging` habilite los adjuntos grandes. |
| 75, 80 | Emojis personalizados y beneficios de servidor boosteado | Su saga de boosts con `servers` es la que sube el nivel de un servidor. |

## Decisiones técnicas

- **ADR 0004, outbox:** el mismo patrón que usa `messaging`, para que el evento no se pierda si RabbitMQ se cae.
