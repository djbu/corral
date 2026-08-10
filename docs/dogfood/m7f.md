# Evidencia M7F — instalación y upgrade

**Fecha:** 2026-08-10 (America/Bogota)
**Alcance:** paso 54 y cierre de M7

## Candidato promovido

- tag anotado: [`v0.7.0-rc.2`](https://github.com/djbu/corral/releases/tag/v0.7.0-rc.2);
- commit del binario candidato: `f798cb0dbd9be8698b0fd28e5eee7f60405f6872`;
- [release verificada](https://github.com/djbu/corral/actions/runs/31403110161):
  cuatro archives, cuatro SBOM, checksums y metadata;
- [matriz M7F verde](https://github.com/djbu/corral/actions/runs/31404825350):
  macOS 1m18s y Ubuntu 1m11s.

El cierre final se publica como tag anotado
[`v0.7.0`](https://github.com/djbu/corral/releases/tag/v0.7.0) desde el commit
que contiene esta evidencia y el renderer Homebrew privado ya corregido.

## Contrato demostrado en ambas plataformas

- se construyó la línea base histórica `v0.6.0`, se creó schema 6 y una sesión
  fake viva con transcript recuperable;
- el instalador autenticado verificó checksum/metadata y reemplazó el binario
  mientras el daemon viejo seguía vivo;
- el CLI nuevo detuvo ordenadamente el daemon viejo, reabrió schema 6 de forma
  idempotente y recuperó la intención `running`;
- restart, kill/wake y attach/detach atravesaron procesos y un PTY reales;
- backup caliente, restore detenido y reinicio conservaron la sesión;
- TLS autofirmado + CA fijada aceptó un token admin y rechazó el mismo token
  después de revocarlo;
- uninstall retiró el binario y conservó DB/directorios de usuario;
- macOS creó un tap efímero, descargó el asset privado por su endpoint API,
  verificó SHA-256, ejecutó `brew install`, `brew test`, uninstall y untap.

## Hallazgos del RC y correcciones

La primera ejecución no se maquilló ni se descartó:

- [`rc.1`](https://github.com/djbu/corral/actions/runs/31402090405) descubrió
  el build Linux no portable del tag histórico y una carrera real entre
  `detach_ack` y EOF; PR
  [#7](https://github.com/djbu/corral/pull/7) corrigió el cliente y documentó
  el backport mínimo de la línea base;
- las primeras ejecuciones `rc.2`
  ([1](https://github.com/djbu/corral/actions/runs/31403450863),
  [2](https://github.com/djbu/corral/actions/runs/31403896316)) demostraron que
  Homebrew actual exige tap y que la URL web de un asset privado responde 404;
  PRs [#8](https://github.com/djbu/corral/pull/8) y
  [#9](https://github.com/djbu/corral/pull/9) hicieron real el tap y cambiaron
  la fórmula a endpoints API autenticados.

El contrato ejecutable vive en `scripts/release/smoke-upgrade.sh` y sus
criterios en `docs/design/m7f.md`.
