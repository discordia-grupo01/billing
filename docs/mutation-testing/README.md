# Mutation testing de billing

## Qué es y para qué sirve

La cobertura dice qué líneas **se ejecutan** durante los tests, pero no si los
tests **revisan** lo que pasa en ellas. Un test que llama a una función y no
mira el resultado suma cobertura y no encuentra ningún error.

El mutation testing mide eso otro. La herramienta mete a propósito errores
chicos en el código, uno por vez (cambia `>` por `>=`, `+` por `-`, niega un
`if`, cambia un `continue` por `break`...), y corre los tests con cada uno.
Cada versión con un error se llama **mutante**:

- Si algún test falla, el mutante quedó **muerto** (`KILLED`): los tests
  detectaron el error. Es lo que queremos.
- Si todos los tests pasan, el mutante **sobrevivió** (`LIVED`): ese error
  podría llegar a producción sin que nada avise.

La **efectividad** es muertos / (muertos + sobrevivientes).

## Herramienta y alcance

Usamos [gremlins](https://github.com/go-gremlins/gremlins), una herramienta de
mutation testing para Go. Las más conocidas (mutmut, Stryker, PIT) son para
Python, JavaScript y Java.

Lo corrimos sobre `internal/service`, donde está la lógica de plata: compra de
puntos, Premium, boosts y la protección contra cobros duplicados. Es donde un
error cuesta más. Sus tests son unitarios (sin base de datos ni RabbitMQ), así
que tardan menos de un segundo y se pueden correr cientos de veces.

Además de los mutantes que gremlins prueba por defecto, activamos todos los
opcionales (invertir operadores lógicos, asignaciones, `continue`/`break`,
operaciones de bits), para que la prueba sea más exigente.

## Cómo correrlo

```bash
go install github.com/go-gremlins/gremlins/cmd/gremlins@latest
make mutation
```

Tarda alrededor de un minuto. No está en el CI porque corre todos los tests
una vez por cada mutante.

**Un detalle que nos pasó:** la primera vez, 163 mutantes dieron `TIMED OUT`.
gremlins calcula cuánto esperar a partir de lo que tardan los tests (0,01 s),
pero con cada mutante tiene que volver a compilar, y eso tarda bastante más.
Con `--timeout-coefficient 50` el problema desapareció. Si se ven muchos
`TIMED OUT`, hay que subir ese número.

## Resultados

Archivos `before.*` (primera corrida) y `after.*` (después de agregar tests).
Los `.txt` son la salida de la terminal y los `.json`, el detalle de cada
mutante.

| | Antes | Después |
|---|---|---|
| Muertos | 157 | 161 |
| Sobrevivientes | 13 | 9 |
| No cubiertos | 14 | 14 |
| Timeout | 1 | 1 |
| **Efectividad** | **92,35%** | **94,71%** |
| Cobertura de líneas | 99,1% | 99,1% |

La cobertura de líneas no cambió: los tests nuevos no ejecutan código nuevo,
pero **revisan** cosas que antes nadie revisaba. Eso es justamente lo que la
cobertura no muestra.

### Los 13 sobrevivientes de la primera corrida

Revisamos uno por uno. Hay dos tipos.

**4 eran tests que faltaban** (ya están agregados):

| Dónde | El error que nadie detectaba | Test nuevo |
|---|---|---|
| `points_service.go:288` | En `ReconcilePending`, después de una compra sin orden, `break` en vez de `continue`: no se revisan las compras que siguen | `TestReconcilePendingGoesOnToTheNextPurchase` |
| `points_service.go:292` | Lo mismo después de una compra que Mercado Pago no pudo consultar | `TestReconcilePendingGoesOnToTheNextPurchase` |
| `points_service.go:317` | Lo mismo en `ReconcilePaid`: los reembolsos y contracargos de las compras siguientes no se revisan | `TestReconcilePaidGoesOnToTheNextPurchase` |
| `premium_service.go:169` | Al empezar un checkout de Premium, si un checkout pendiente no existe en Mercado Pago, se dejan de cancelar los que siguen, y **se podrían pagar dos suscripciones** | `TestPremiumStartCheckoutCancelsPendingAfterAnUnknownOne` |

Ya había tests que se llamaban "sigue después de un error"
(`TestReconcilePendingKeepsGoingAfterErrors`), pero usaban **una sola compra**.
Con una sola compra, `continue` y `break` hacen lo mismo. Los tests nuevos usan
dos: la primera falla y se comprueba que la segunda igual se procese.

Para eso tuvimos que hacer dos cambios en los fakes de los tests: el gateway
falso ahora puede fallar con una orden puntual (`failOn["GetPointsOrder ORD0"]`),
y el store falso devuelve las compras pagadas siempre en el mismo orden (como
la base). Si no, el test detectaría el error unas veces sí y otras no.

**9 son mutantes equivalentes**: el cambio no altera nada que se pueda
comprobar, así que no hay test posible. Siguen apareciendo como `LIVED`:

| Dónde | Por qué es equivalente |
|---|---|
| `points_service.go:239` | Solo decide si se escribe un aviso en el log; los puntos se acreditan igual |
| `points_service.go:285`, `297`, `319` | Solo cambian cuándo se loguea un error al guardar |
| `premium_service.go:223`, `412` | Ídem |
| `checkout_guard.go:74`, `79` | Ídem |
| `boost_service.go:58` | Solo cambia qué logger se usa |

Sin contar los equivalentes, los tests detectan **todos** los mutantes que
cambian el comportamiento.

### Lo que no cuenta

- **El timeout** (`premium_service.go:389`): el mutante convierte el bucle de
  `ExpireDue` en infinito, así que el test no termina. En la práctica cuenta
  como detectado.
- **Los 14 no cubiertos** no son código sin tests. 10 son constantes
  (`24 * time.Hour`), que la cobertura de Go no mide. Los otros 4 son
  condiciones de `switch` (`checkout_guard.go:61` y `63`,
  `premium_service.go:249` y `265`): la cobertura de Go mide el cuerpo de cada
  `case` pero no su condición, y esos cuerpos sí están cubiertos. Es una
  limitación de la herramienta.
