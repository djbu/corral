# M7F — Smoke de instalación y upgrade

## Propósito

M7F cierra M7 con evidencia sobre sistemas limpios, no con una inferencia a
partir de unit tests. El candidato debe poder reemplazar `v0.6.0` conservando
el estado real, abrir/aplicar migraciones idempotentemente y seguir operando por el
socket local como por TLS autenticado.

## Matriz y artefacto bajo prueba

El workflow manual `upgrade-smoke.yml` usa runners efímeros
`macos-latest` y `ubuntu-latest`. Recibe únicamente un tag anotado
`v0.7.0-rc.N`; el instalador descarga de la release privada el archive de la
plataforma y su archivo de checksums. Por tanto, el binario probado es el
artefacto publicado, no una compilación sustituta del checkout.

La línea base sí se compila directamente desde el tag histórico anotado
`v0.6.0`, con su identidad de versión y commit embebida. Tanto `$HOME` como el
prefix, state dir, socket, fake Claude y workspace viven dentro del temporal
del runner.

## Contrato ejecutado

1. Construir e instalar `v0.6.0`, iniciar el daemon y crear una sesión
   `fakeclaude` que permanece viva; enviar un turno por attach para dejar una
   transcripción realmente recuperable.
2. Instalar el RC sobre el mismo path mientras el proceso viejo sigue vivo.
3. Usar el CLI nuevo para solicitar shutdown limpio al daemon viejo; arrancar
   el RC, comprobar que schema 6 se reabre idempotentemente, intención
   `running` y una nueva invocación de
   recuperación.
4. Ejecutar restart, kill/wake y attach/detach. Attach corre dentro de un PTY
   real, espera el banner de fakeclaude y envía la secuencia configurada
   `C-\\ d`.
5. Crear un backup caliente, detener, restaurar con rollback automático y
   volver a iniciar la sesión.
6. Reiniciar con listener loopback TLS, crear un token local, listar por el
   cliente remoto con CA fijada, revocar el token y demostrar que deja de ser
   aceptado.
7. Desinstalar el binario y demostrar que la DB y los archivos de sesión se
   conservan. En macOS, materializar además la fórmula desde los checksums
   publicados e instalar/probar/desinstalar con Homebrew y token privado.

Cada espera consulta un estado observable o un archivo de invocación, con
timeout. Los sleeps sólo espacian polls; no se usan como evidencia de éxito.

## Gate y promoción

El RC no se promueve si cualquiera de las dos plataformas falla, si el tag no
es anotado, si CI no está verde para su commit o si release no publica
artefactos verificables. Tras dos jobs verdes se registra la ejecución en
`docs/dogfood/m7f.md`, se actualizan los manuales y roadmap, y el mismo commit
se etiqueta de forma anotada como `v0.7.0`. El pipeline vuelve a producir y
verificar la release final; sus checksums y SBOM son la evidencia de cierre.
