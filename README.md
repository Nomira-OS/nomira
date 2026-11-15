# nomira

Ein BuildKit-Gateway-Frontend für phasen-basierte, **verifiziert-parallele**
OCI-Builds von bootc-Systemimages (primär Arch Linux). Siehe `docs/concept.md`
(Konzept v1; beim Recipe-Schema gilt dieses README) für Motivation und Modell.

- **Phasen** laufen seriell in deklarierter Reihenfolge.
- **Gruppen** innerhalb einer Phase sind deklariert unabhängig: Fan-out auf
  demselben Basiszustand, parallel gebaut, einzeln gecacht (DiffOp), am
  Phasenende zusammengeführt (MergeOp).
- Die Unabhängigkeit wird **im Build verifiziert** (Write-Set-Disjunktheit
  mit Whiteout-Präfix-Semantik). Ein Konflikt bricht den Build ab, bevor
  gemergt oder exportiert wird.

```
S ──Exec(g₁)──→ R₁ ──Diff──→ Δ₁  ┐
S ──Exec(g₂)──→ R₂ ──Diff──→ Δ₂  ├─ parallel, einzeln gecacht, validiert
S ──Exec(gₙ)──→ Rₙ ──Diff──→ Δₙ  ┘
S' = Merge(S, Δ₁ … Δₙ)
```

## Recipe

```yaml
# syntax=ghcr.io/leaced/nomira:1
settings:
  name: meinsystem
  base: ghcr.io/…/arch-bootc@sha256:…
  validation: strict                # Datei-Default: strict | allow-identical
  args:
    SNAPSHOT: "2026/11/10"          # ${SNAPSHOT} in Befehlen; --build-arg überschreibt

prepare:                            # optionale benannte Stages (Multistage)
  aurbuild:
    from: docker.io/library/archlinux:base-devel
    commands:
      - run pacman -Syu --noconfirm git

phases:                             # Liste → Reihenfolge zählt
  - name: base
    groups:                         # Mapping → Gruppen unabhängig, Key = Name
      system:                       # einzelne Gruppe = serieller Fall, keine Validierung
        - run --mount=type=cache,target=/var/cache/pacman/pkg pacman -Syu --needed --noconfirm kmscon
  - name: configs
    validation: allow-identical     # Phase überschreibt Default
    shared: [/usr/lib/tmpfiles.d/]  # Allowlist (Suffix "/" = Präfix)
    groups:
      kmscon:
        - copy --chmod=0644 files/kmscon/ /etc/kmscon/
      aur:
        - copy --from=aurbuild /out/ /var/tmp/aur/
```

Befehle sind Strings mit Verb am Anfang (Mehrzeiler als YAML-Blockskalar):

| Befehl | Flags |
|---|---|
| `run <shell>` | `--mount=type=cache,target=/pfad[,id=…][,sharing=shared\|private\|locked]` |
| `copy <src>… <dst>` | `--from=<stage>`, `--chmod=0644`, `--chown=UID:GID` |

`${NAME}` wird nur für in `settings.args` deklarierte Args ersetzt; `$repo`
u. ä. bleiben unangetastet. Ein geänderter Arg invalidiert genau die
Befehle, die ihn verwenden (korrekte Cache-Semantik).

## Bauen eines Recipes

Das Frontend-Image wird von der CI (`.github/workflows/ci.yml`) bei jedem
Push auf `main` nach `ghcr.io/leaced/nomira` veröffentlicht (Tags `1`,
`latest`, `sha-…`; bei `v*`-Tags zusätzlich die Versionsnummer), für
`linux/amd64` und `linux/arm64`. Lokal bauen geht weiterhin:

```sh
podman build -f Dockerfile.frontend -t ghcr.io/leaced/nomira:1 .
```

Dann baut jedes Vanilla-BuildKit das Recipe — die `# syntax=`-Zeile wählt
das Frontend selbst aus:

```sh
buildctl build \
  --frontend dockerfile.v0 \
  --local context=. --local dockerfile=. \
  --opt filename=recipe.yml \
  --opt build-arg:SNAPSHOT=2026/12/01 \
  --output type=oci,dest=img.tar
```

Oder explizit, ohne `# syntax`-Zeile (praktisch zum Testen lokaler
Frontend-Builds):

```sh
buildctl build \
  --frontend gateway.v0 --opt source=ghcr.io/leaced/nomira:1 \
  --local context=. --local dockerfile=. \
  --opt filename=recipe.yml \
  --output type=oci,dest=img.tar
```

Lieferung an podman/bootc:

```sh
skopeo copy oci-archive:img.tar containers-storage:localhost/meinsystem
bootc switch --transport containers-storage localhost/meinsystem
```

## Zusatzmodi (für dünne CLIs)

Dasselbe Binary dient als Parser-Orakel, damit eine CLI in beliebiger
Sprache (z. B. Rust) Schema-Validierung und Graph nicht nachbauen muss:

```sh
nomira-frontend lint  recipe.yml     # Parse + Schema; Exit 1 mit Fehlern
nomira-frontend graph recipe.yml     # Pipeline als Mermaid-Flowchart
podman run --rm -i ghcr.io/leaced/nomira:1 lint - < recipe.yml
```

## Entwicklung

Mit normalem Netz genügt:

```sh
go mod tidy && make test build
```

`hack/gen_offline_deps.py` existiert für Umgebungen ohne Go-Modul-Proxy:
Es erzeugt eine `go.offline.mod`, die alle Abhängigkeiten per `replace` aus
einem lokalen `moby/buildkit`-Checkout (dessen `vendor/`-Baum) bedient;
gebaut wird dann mit `GOFLAGS="-mod=mod -modfile=go.offline.mod"`. Das
normale `go.mod` bleibt davon unberührt.

Tests: `go test ./...` (Parser, Konfliktprüfung) plus
`hack/test_lister.sh`, der `internal/compile/lister.sh` gegen synthetische
Bäume prüft (Add/Change/Metadaten/Symlink/Datei- und Verzeichnis-Löschung/
Leerzeichen-Pfade).

## Status / bekannte Grenzen (v1-Gerüst)

- Write-Set-Erhebung hasht den gesamten Upper-Baum einer Gruppe; Phasen mit
  nur einer Gruppe (z. B. die pacman-Basisphase) überspringen Diff, Merge
  und Validierung vollständig.
- `copy` unterstützt keine Pfade mit Leerzeichen in der Befehlszeile (die
  Validierung selbst schon); `--chown` nur numerisch.
- Xattrs/Hardlinks werden vom Lister noch nicht als eigene Writes erfasst
  (concept.md §10).
- Der Lister braucht ein Toolbox-Image mit busybox-Werkzeugen
  (`settings.toolbox`, Default `docker.io/library/busybox:1.36`).
- Plattform folgt dem Build-Host; Multi-Arch ist offen.
- Gegen BuildKit v0.35.0 gebaut; MergeOp/DiffOp brauchen ≥ 0.10.
