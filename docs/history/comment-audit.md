# Comment audit

This is a historical snapshot of the first comment cleanup, not the current
comment policy. See [Contributing](../../CONTRIBUTING.md#code-comments) for the
current checks.

Removing prose comments saves **3,180 whole lines**. Another **110 comments** shared lines with code, so removing them saves characters only. These gross savings exclude the text added to the decision records, this audit and the linter.

The audit covers 3,326 physical comment lines in 172 files at the pre-cleanup revision. Counts include comments inside executable examples and embedded scripts. Interpreter, compiler and lint directives retain their behavior. Python docstrings are runtime strings. Repository prose and comment-like text used as fixture data remain outside the counts. [Baseline source tree](https://github.com/ruhuang2001/tiny-sandbox/tree/0622cb2c369abe4374684ada7c67191e87780e87)

| Classification | Whole lines | Shared with code | Total lines |
| --- | ---: | ---: | ---: |
| Tautology | 760 | 42 | 802 |
| Stale | 381 | 3 | 384 |
| Decision Record | 2,026 | 65 | 2,091 |
| Excuse | 13 | 0 | 13 |
| Directive | 35 | 1 | 36 |
| **Total** | **3,215** | **111** | **3,326** |

Tautologies duplicated declarations or assertions. Stale comments described behavior the implementation had outgrown. The decision records preserve the reasons for implementation and test choices. [Daemon](../adr/0001-daemon.md) · [Backend and CLI](../adr/0002-backend-cli.md) · [SDKs](../adr/0003-sdks.md) · [Acceptance](../adr/0004-acceptance.md)

Four comments described unresolved implementation compromises. Each now has a tracking issue.

- The environment separator can overwrite caller-supplied shell variables when the executable name contains an equals sign. [Issue 21](https://github.com/ruhuang2001/tiny-sandbox/issues/21)
- Admission while the reaper awaits confirmation of container removal. [Issue 22](https://github.com/ruhuang2001/tiny-sandbox/issues/22)
- The CLI can discard a sandbox cleanup failure while reporting the command outcome. [Issue 23](https://github.com/ruhuang2001/tiny-sandbox/issues/23)
- Container creation can remove a conflicting name without verifying container ownership. [Issue 24](https://github.com/ruhuang2001/tiny-sandbox/issues/24)

## Source locations

Line numbers below refer to the linked pre-cleanup files. Each range is inclusive. Adjacent lines with the same classification are combined, even when they belonged to separate comment blocks.

### Build, configuration and documentation

| Pre-cleanup file | Tautology | Stale | Decision Record | Excuse | Directive |
| --- | --- | --- | --- | --- | --- |
| [.githooks/commit-msg](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/.githooks/commit-msg) | — | — | — | — | 1 |
| [.github/workflows/sdk-publish.yml](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/.github/workflows/sdk-publish.yml) | — | — | 43 | — | — |
| [.gitignore](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/.gitignore) | 6, 13, 16, 24 | — | 1–2, 9 | — | — |
| [Makefile](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/Makefile) | 27, 31, 35, 39 | — | — | — | — |
| [docs/sdk-python.md](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/docs/sdk-python.md) | — | — | 172 | — | — |
| [docs/sdk-typescript.md](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/docs/sdk-typescript.md) | 19 | — | — | — | — |
| [docs/sdk.md](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/docs/sdk.md) | 44, 52, 64–65, 68 | — | 28–31, 194–195, 197–198, 207–213 | — | — |

### API, commands and examples

| Pre-cleanup file | Tautology | Stale | Decision Record | Excuse | Directive |
| --- | --- | --- | --- | --- | --- |
| [api/contract.go](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/api/contract.go) | 22–23, 35, 40, 42–43, 46, 51–52, 81–82 | — | 1–7 | — | 19 |
| [api/contract_test.go](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/api/contract_test.go) | 142–143, 146–147, 208–209, 218–220 | 240–241 | 123–125, 174–177 | — | — |
| [cmd/sandboxctl/cli_test.go](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/cmd/sandboxctl/cli_test.go) | 1, 34, 41, 127, 400, 518–519, 529 | 172–173, 176–177, 241, 390, 402–403 | 16–18, 244, 450–452, 472–474, 490–493, 539 | — | — |
| [cmd/sandboxctl/doctor.go](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/cmd/sandboxctl/doctor.go) | 17–18, 42, 82, 88, 107, 165, 190 | 60, 136–138 | 54, 75, 118–121, 124–130, 141–143, 158–159, 194–196 | — | — |
| [cmd/sandboxctl/files.go](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/cmd/sandboxctl/files.go) | — | — | 98, 123 | — | — |
| [cmd/sandboxctl/main.go](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/cmd/sandboxctl/main.go) | 128, 135, 148, 155, 165, 170–171, 214, 235, 248, 250–251, 257, 273, 288–289, 301–302 | — | 76–77, 85–86, 180–181, 188–190, 200–202, 275–278, 316–317 | — | — |
| [cmd/sandboxctl/ops.go](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/cmd/sandboxctl/ops.go) | 40–41, 72, 113–114, 134 | — | 10–11, 164–165 | — | — |
| [cmd/sandboxctl/run.go](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/cmd/sandboxctl/run.go) | 45, 64, 247 | 257 | 17–23, 48–50, 133–134, 230–231, 273–275, 313–314, 350–352 | 239–240 | — |
| [cmd/sandboxd/main.go](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/cmd/sandboxd/main.go) | 1 | 174–175 | 76–77, 105–107, 119–122, 133, 148–149, 153–154, 187–188 | — | — |
| [examples/agent-tool/README.md](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/examples/agent-tool/README.md) | 40 | — | 43 | — | — |
| [examples/agent-tool/cmd/demo/main.go](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/examples/agent-tool/cmd/demo/main.go) | 63–64 | 1–11, 78 | 55–56, 71–73, 89–91 | — | — |
| [examples/agent-tool/tool.go](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/examples/agent-tool/tool.go) | 25, 28–29, 36–37, 62, 72, 77, 79, 82, 84, 86, 90, 203, 273 | 74–75, 97–98, 134–135 | 1–12, 68–70, 103–105, 124–125, 148–149, 211–212 | — | — |
| [examples/agent-tool/tool_test.go](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/examples/agent-tool/tool_test.go) | 118–119, 148, 259–260, 270–271 | — | 17–18, 60, 94, 108–110, 250 | — | — |
| [examples/e2b/Dockerfile](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/examples/e2b/Dockerfile) | — | — | 1–2 | — | — |
| [examples/e2b/README.md](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/examples/e2b/README.md) | — | — | 18 | — | — |

### Internal packages

| Pre-cleanup file | Tautology | Stale | Decision Record | Excuse | Directive |
| --- | --- | --- | --- | --- | --- |
| [internal/daemon/admit_race_test.go](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/internal/daemon/admit_race_test.go) | 38–39 | — | 11–14 | — | — |
| [internal/daemon/backend.go](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/internal/daemon/backend.go) | 10–11, 14–15, 42, 44, 46–47, 49, 53, 58, 74, 82, 84–85, 87–88, 96, 118–119, 139–140, 142–143, 145, 147, 157, 171, 173 | 90–92, 120–122, 137, 159–161 | 32–38, 78–80, 98–100, 102–105, 107–110, 112, 114, 123–124, 132–136, 149–155, 163–164, 166–167, 169 | — | — |
| [internal/daemon/cancel_test.go](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/internal/daemon/cancel_test.go) | 210 | 11–14, 173–174 | 199–202 | — | — |
| [internal/daemon/classify.go](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/internal/daemon/classify.go) | 8–9, 15, 17, 129–130 | 27–31, 36–46, 76–89 | 19–20, 22–24, 61–63, 123–125, 134–136 | — | — |
| [internal/daemon/classify_test.go](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/internal/daemon/classify_test.go) | 13–14, 45, 52–53, 67–69, 105, 108–110, 210, 212 | 175–180, 224–227 | 137–139, 205–207, 245–246 | — | — |
| [internal/daemon/commands.go](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/internal/daemon/commands.go) | 26 | — | 10–11, 20, 45–46, 68–69, 79, 108–109 | — | — |
| [internal/daemon/doc.go](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/internal/daemon/doc.go) | — | 1–6 | — | — | — |
| [internal/daemon/engine.go](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/internal/daemon/engine.go) | 63–64, 151, 156, 209, 420–421, 563, 676–677, 699, 701, 710–711, 731, 752, 791–793, 802, 807–808, 817, 819–820, 843, 878–879, 969, 1002–1003 | 70–71, 211–216, 493–494, 565–571, 723–725 | 14–16, 21–25, 32, 43, 54–56, 59–61, 74–75, 84–85, 108, 126, 188–190, 263–265, 303–312, 321–325, 364–366, 404–405, 436–441, 456–457, 487–489, 501–508, 517, 523–528, 536–537, 541–543, 553–554, 581–582, 592, 595–600, 611–612, 614, 616–617, 622–631, 637–639, 646–649, 672, 754–756, 764–771, 904–906, 936–941, 956–959, 991–994 | — | — |
| [internal/daemon/env_cwd_test.go](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/internal/daemon/env_cwd_test.go) | 13–16, 26, 60–62, 80, 240–242, 270–273, 277–278, 323–326 | 248–249 | 112–114, 145–147, 157–158, 172–174, 196–197 | — | — |
| [internal/daemon/env_fingerprint.go](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/internal/daemon/env_fingerprint.go) | 15 | — | 10–12, 19–21 | — | — |
| [internal/daemon/errors.go](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/internal/daemon/errors.go) | 8, 11, 26–27, 41, 48–49, 58–59, 66–67 | — | 62–63 | — | — |
| [internal/daemon/errors_test.go](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/internal/daemon/errors_test.go) | 84, 107, 131, 141–142, 251 | 28 | 13–15, 188–192, 217–219, 230–231 | — | — |
| [internal/daemon/events.go](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/internal/daemon/events.go) | 10, 13, 21, 23, 25, 27, 33–34, 51, 69, 118, 142–145, 176, 191, 214–215, 234, 241, 256, 270 | 180 | 29–31, 38–48, 75–86, 97–99, 158–161, 199–200, 207–210, 248–249 | — | — |
| [internal/daemon/events_test.go](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/internal/daemon/events_test.go) | 14, 32–34, 71, 181–183, 195, 305, 331–332, 377, 410 | — | 321–324, 356–357, 392–395, 421–423, 452 | — | — |
| [internal/daemon/exec_test.go](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/internal/daemon/exec_test.go) | 14, 81–82, 115, 170 | — | 124–126, 144–146 | — | — |
| [internal/daemon/export_test.go](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/internal/daemon/export_test.go) | 8–9, 29, 42–43, 56 | — | 20–23, 50–51 | — | — |
| [internal/daemon/file_archive.go](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/internal/daemon/file_archive.go) | 15 | — | 40 | — | — |
| [internal/daemon/file_list.go](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/internal/daemon/file_list.go) | 45, 48, 58, 110 | 17, 64 | 25–30, 53–54, 77 | — | — |
| [internal/daemon/handler.go](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/internal/daemon/handler.go) | 63–64, 69, 118, 128, 152, 188–189, 215, 234, 280, 282, 326, 336, 338–339, 463–465, 485, 487–488, 526 | 20–21 | 26–29, 34–40, 47–48, 54–57, 75–76, 133–136, 298–300, 361–364, 388–394, 396–398, 413–415, 443–444, 504, 512 | — | — |
| [internal/daemon/handler_test.go](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/internal/daemon/handler_test.go) | 21, 37, 44, 73, 78, 86, 132, 136, 149, 222, 243, 254, 268, 274, 285, 294, 299, 340, 369, 376–377, 393, 441, 448, 455, 479, 526 | — | 470–472 | — | — |
| [internal/daemon/harness_test.go](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/internal/daemon/harness_test.go) | 39, 69, 98, 136, 145–146, 158, 169–170, 176–177, 182–183, 185, 212 | 36 | 13–14, 116–119 | — | — |
| [internal/daemon/lifecycle_test.go](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/internal/daemon/lifecycle_test.go) | 11–13, 17, 136, 258, 266, 271, 288 | — | 225–227, 307–308 | — | — |
| [internal/daemon/membackend/file_archive.go](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/internal/daemon/membackend/file_archive.go) | 16, 130 | — | 216 | — | — |
| [internal/daemon/membackend/file_list.go](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/internal/daemon/membackend/file_list.go) | 13 | — | — | — | — |
| [internal/daemon/membackend/membackend.go](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/internal/daemon/membackend/membackend.go) | 27–29, 32–34, 37, 52, 55, 58–59, 96, 108, 110, 167, 243, 254, 265, 298, 330, 332–334, 362–363, 368–369, 400, 420, 435, 438, 441–443, 455, 483, 490, 492, 495, 498, 506–507, 546, 577, 597, 611, 624 | 43–45, 49–50, 135, 152, 347–349, 387–388, 465–466 | 1–9, 182–185, 190–191, 237–238, 409, 519–522 | — | — |
| [internal/daemon/metrics.go](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/internal/daemon/metrics.go) | — | — | 13–14 | — | — |
| [internal/daemon/observability.go](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/internal/daemon/observability.go) | — | — | 19, 67 | — | — |
| [internal/daemon/output_bench_test.go](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/internal/daemon/output_bench_test.go) | 5 | — | — | — | — |
| [internal/daemon/policy.go](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/internal/daemon/policy.go) | 25, 38, 76, 91, 97, 103, 111, 130 | — | 14–15, 59, 67–68, 94, 133, 173, 184 | — | — |
| [internal/daemon/reaper.go](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/internal/daemon/reaper.go) | 17, 77, 137, 152, 183–184, 200, 211–212, 219 | 159–170, 261–264, 340–344 | 9–12, 28–36, 52–58, 85–89, 96–97, 110–111, 125–130, 144–147, 191, 226–227, 255–256, 276–278, 331 | 244–247 | — |
| [internal/daemon/recovery.go](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/internal/daemon/recovery.go) | — | — | 19, 24–25, 60–61, 114, 136, 233–234, 250–252, 294–295, 381–382, 418, 423–424, 463–464, 475 | — | — |
| [internal/daemon/retention.go](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/internal/daemon/retention.go) | 26, 97, 104, 163–164 | 5–24, 48–49, 61–67 | 32–34, 113–116, 137–138, 151 | — | — |
| [internal/daemon/retention_test.go](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/internal/daemon/retention_test.go) | 11–13, 59, 63, 76, 106 | 28, 112–114 | 37–40, 71–73, 99–100 | — | — |
| [internal/daemon/routes.go](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/internal/daemon/routes.go) | 11–12, 109–110 | 63–65 | 18–21, 24–26, 32–33, 74–76, 81–82 | — | — |
| [internal/daemon/routes_test.go](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/internal/daemon/routes_test.go) | 52–53 | 38–40 | 8–10, 24–27 | — | — |
| [internal/daemon/store.go](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/internal/daemon/store.go) | 17, 22–24, 27, 30, 33–34, 39–40, 47, 50–51, 54–55, 58, 63, 68–69, 115, 134, 174, 189, 205–207, 213, 237, 239, 368–369, 376, 412, 414, 496, 564–565, 600–601, 676–677, 754, 795, 803, 815–816, 850, 855–856, 869 | 11–12, 43–44, 72–78, 82–83, 88–95, 116–118, 186, 307–309, 425–426, 685–688, 698, 756–762, 781 | 121–123, 129–131, 199–200, 209–210, 220–228, 315–317, 331–333, 342–343, 353–356, 381, 441–443, 457, 464–466, 498–504, 531–532, 544–545, 589–590, 623–625, 638–645, 666–668, 724–734, 737, 748–749, 797–799, 828–831 | — | — |
| [internal/daemon/timeout_test.go](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/internal/daemon/timeout_test.go) | 17–19, 82–83, 120, 203, 289–290, 325, 334–335, 467–468, 484–485, 601, 677, 704 | 194–196 | 131–132, 224–225, 297–298, 592, 613 | — | — |
| [internal/daemon/types.go](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/internal/daemon/types.go) | 10, 31, 33, 35, 43, 45, 53, 55, 86–89, 93, 96, 104, 107, 115, 123, 128–129, 133, 154, 157, 163, 182, 199–200, 215, 322, 334–335, 339, 346, 433 | 37, 50–51, 68–71, 78–81, 139–141, 144, 437–438 | 17–20, 22–24, 26–28, 39–41, 47–48, 57, 59, 61–62, 64–66, 73–76, 83–84, 118–120, 125–126, 232–233, 245–247, 251–253, 255–257, 259–262, 266–278, 289–292, 305–307 | — | — |
| [internal/dockerapi/archive.go](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/internal/dockerapi/archive.go) | 77 | — | 14, 17–19, 24–29, 83–84, 100 | — | — |
| [internal/dockerapi/archive_test.go](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/internal/dockerapi/archive_test.go) | — | — | 38 | — | — |
| [internal/dockerapi/cgroup.go](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/internal/dockerapi/cgroup.go) | — | — | 9–13, 18–36 | — | — |
| [internal/dockerapi/cgroup_test.go](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/internal/dockerapi/cgroup_test.go) | — | — | 30–31 | — | — |
| [internal/dockerapi/client.go](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/internal/dockerapi/client.go) | 25, 32–33, 68, 94–95, 116, 118, 137, 142, 148, 157, 162, 199, 256, 274–275, 292, 308, 321 | 1–7, 167–168, 258–259 | 28, 30, 37–38, 46–49, 98–100, 177–179, 201–203, 222–227, 246–247 | — | — |
| [internal/dockerapi/errors.go](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/internal/dockerapi/errors.go) | 40–41, 47–48 | — | 12–13, 26–27 | — | — |
| [internal/dockerapi/fuzz_test.go](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/internal/dockerapi/fuzz_test.go) | 24–28, 77–78 | — | 9–19, 48–52, 62–66, 95–96 | — | — |
| [internal/dockerapi/image.go](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/internal/dockerapi/image.go) | — | — | 16–28, 47–48, 59–60, 67–69, 81, 95–96 | — | — |
| [internal/dockerapi/image_test.go](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/internal/dockerapi/image_test.go) | — | — | 51 | — | — |
| [internal/dockerapi/inspect.go](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/internal/dockerapi/inspect.go) | 14 | — | 3–6 | — | — |
| [internal/dockerapi/stream.go](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/internal/dockerapi/stream.go) | — | — | 10–11, 19–27, 45, 47, 55–57 | — | — |
| [internal/dockerapi/stream_test.go](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/internal/dockerapi/stream_test.go) | 12–13, 93–94 | — | 76–77 | — | — |
| [internal/dockerapi/types.go](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/internal/dockerapi/types.go) | 36, 46–47 | — | 3–4, 13–19, 29–32, 50, 56, 61–76 | — | — |
| [internal/dockerapi/types_test.go](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/internal/dockerapi/types_test.go) | — | — | 28–31, 55–56, 62–63 | — | — |
| [internal/dockerbackend/backend.go](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/internal/dockerbackend/backend.go) | 72, 84–85, 88, 101, 104, 163, 172, 234, 249–250, 273, 385, 502, 504, 567–568, 657–658, 699–700 | 38–40, 178–179, 275–278 | 1–16, 43–48, 51–55, 58–64, 67–69, 75, 106–107, 122, 135–137, 155–156, 185–191, 213–215, 300, 314–316, 329–330, 357–359, 366–368, 387–389, 408–416, 430–433, 444, 447–458, 475–476, 481–483, 513–514, 595–629, 639–649, 663–665, 667–668, 684–686, 711–713, 723–724 | 144–145, 650–654 | — |
| [internal/dockerbackend/backend_test.go](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/internal/dockerbackend/backend_test.go) | 86–88, 217, 300, 334, 680 | — | 42–43, 59–60, 67–69, 73–74, 117–119, 150–152, 232–233, 255–256, 343, 391–392, 646, 672 | — | — |
| [internal/dockerbackend/background_acceptance_test.go](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/internal/dockerbackend/background_acceptance_test.go) | — | — | 14–17, 78 | — | 1 |
| [internal/dockerbackend/cleanup.go](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/internal/dockerbackend/cleanup.go) | — | — | 14–15, 21, 26–29, 35, 44, 70–73, 120, 145–147, 164–165 | — | — |
| [internal/dockerbackend/cleanup_test.go](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/internal/dockerbackend/cleanup_test.go) | — | — | 38–39 | — | — |
| [internal/dockerbackend/docker_test.go](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/internal/dockerbackend/docker_test.go) | 24, 96, 106, 116, 137, 202, 256, 270–271, 279–280, 326, 527 | — | 3–9, 33–34, 129–131, 145, 164–166, 172–173, 181–182, 216–218, 229–232, 290–297, 309, 315–316, 332–339, 350–351, 373–375, 387–388, 401–408, 433–453, 468–469, 500–502, 514–516 | — | 1 |
| [internal/dockerbackend/exec_spec_test.go](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/internal/dockerbackend/exec_spec_test.go) | — | — | 15–18, 50–56, 82–83, 95–99, 106–110, 115–116, 128–133, 144–146, 160–163, 198–204, 207–208, 224–226 | — | — |
| [internal/dockerbackend/fake_test.go](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/internal/dockerbackend/fake_test.go) | 35, 45, 61, 77, 123, 195, 204, 218, 365–366, 410–411, 420, 424–425, 439, 602, 612, 622, 632 | 227, 299–302, 401–402 | 25–26, 50–51, 80–82, 108, 160–161, 178–179, 269–270, 339, 352–353, 572–579 | — | — |
| [internal/dockerbackend/file_archive.go](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/internal/dockerbackend/file_archive.go) | — | — | 12, 29, 91 | — | — |
| [internal/dockerbackend/file_list.go](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/internal/dockerbackend/file_list.go) | — | — | 19–21, 25, 69–70, 238 | — | — |
| [internal/dockerbackend/managed.go](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/internal/dockerbackend/managed.go) | — | — | 19–21, 39–40, 97–98, 105–106 | — | 16 |
| [internal/dockerbackend/oom.go](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/internal/dockerbackend/oom.go) | — | — | 10–11 | — | — |
| [internal/dockerbackend/oom_real_test.go](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/internal/dockerbackend/oom_real_test.go) | — | — | 11–12 | — | 1 |
| [internal/dockerbackend/oom_test.go](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/internal/dockerbackend/oom_test.go) | — | — | 38, 45 | — | — |
| [internal/dockerbackend/policy.go](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/internal/dockerbackend/policy.go) | — | — | 13–14, 24–25 | — | — |
| [internal/dockerbackend/process.go](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/internal/dockerbackend/process.go) | 13–14, 23, 34–35, 38 | 100 | 41–47, 70–75, 97–98, 106–112 | — | — |
| [internal/dockerbackend/stdin.go](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/internal/dockerbackend/stdin.go) | — | — | 12–14, 20–21, 52–53, 67 | — | — |
| [internal/dockerbackend/stdin_real_test.go](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/internal/dockerbackend/stdin_real_test.go) | — | — | 28 | — | 1 |
| [internal/dockerbackend/stream.go](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/internal/dockerbackend/stream.go) | — | 33–34, 41–42 | 9–12, 15–19, 59–60, 72, 76–77 | — | — |
| [internal/dockerbackend/supervisor.py](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/internal/dockerbackend/supervisor.py) | — | — | 24–25, 53–56, 73–75, 114–115, 150–151 | — | — |
| [internal/filearchive/archive.go](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/internal/filearchive/archive.go) | 1, 24, 34, 40, 220 | — | 15, 46, 208, 217 | — | — |
| [internal/filearchive/archive_test.go](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/internal/filearchive/archive_test.go) | 76 | — | — | — | — |
| [internal/wirejson/wirejson.go](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/internal/wirejson/wirejson.go) | 17 | — | 1–2, 11, 14 | — | — |

### SDKs

| Pre-cleanup file | Tautology | Stale | Decision Record | Excuse | Directive |
| --- | --- | --- | --- | --- | --- |
| [sandbox-go/client.go](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/sandbox-go/client.go) | 28, 40 | — | 1–8, 19–20, 22–25, 34–35, 37 | — | — |
| [sandbox-go/client_test.go](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/sandbox-go/client_test.go) | 102, 389 | 25, 394–397 | 124–125, 142–143, 232–234, 245–246, 266, 279–280, 579 | — | — |
| [sandbox-go/commands.go](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/sandbox-go/commands.go) | — | — | 12, 52, 57, 80, 86, 92, 111, 117–119 | — | — |
| [sandbox-go/errors.go](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/sandbox-go/errors.go) | 42, 54 | 30–32, 100–102, 125–128 | 9–15, 34–35, 37–38, 57–59, 86–89, 112–113 | — | — |
| [sandbox-go/exec.go](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/sandbox-go/exec.go) | — | — | 15–22, 27, 67–68, 86–88, 127, 144–145, 162–164, 185, 191 | — | — |
| [sandbox-go/files.go](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/sandbox-go/files.go) | 14, 17 | — | 25, 40–41, 68, 74, 80, 106, 118–119 | — | — |
| [sandbox-go/fuzz_test.go](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/sandbox-go/fuzz_test.go) | 32–37, 56–57 | 10–27, 93–94 | 79–81 | — | — |
| [sandbox-go/live_concurrency_test.go](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/sandbox-go/live_concurrency_test.go) | — | — | 18–19 | — | 1 |
| [sandbox-go/live_env_cwd_test.go](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/sandbox-go/live_env_cwd_test.go) | 21–22, 63, 121–122, 207–208 | 165–167 | 3–8, 33–34, 50–51, 87–95, 107–108, 127–128, 141–149, 201–202 | — | 1 |
| [sandbox-go/live_env_recovery_test.go](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/sandbox-go/live_env_recovery_test.go) | — | — | 42–43 | — | 1 |
| [sandbox-go/live_stdin_test.go](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/sandbox-go/live_stdin_test.go) | — | — | 15 | — | 1 |
| [sandbox-go/live_test.go](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/sandbox-go/live_test.go) | — | — | 3–9, 166 | — | 1 |
| [sandbox-go/retry.go](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/sandbox-go/retry.go) | 62, 83, 85, 97–99, 178–179, 193, 206, 221 | 17–25, 31–33, 80–81, 116–117 | 42–43, 48–49, 72–74, 89–90, 132–133 | — | — |
| [sandbox-go/sandbox.go](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/sandbox-go/sandbox.go) | 9, 35, 63, 86 | — | 11–12, 40, 53, 58, 68–69, 100 | — | — |
| [sandbox-go/stream.go](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/sandbox-go/stream.go) | 171, 226, 297, 306, 315–316, 335 | 177–178, 183–192, 201, 253 | 17–26, 94–95, 103–104, 125–127, 149–150, 173–174, 247, 261–265, 308–310 | — | — |
| [sandbox-go/stream_parse_test.go](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/sandbox-go/stream_parse_test.go) | — | — | 10–13, 32–34 | — | — |
| [sandbox-go/stream_test.go](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/sandbox-go/stream_test.go) | 63, 70, 111, 388, 430, 493 | 83, 95–96, 421 | 19–20, 43–44, 113–115, 139–140, 149, 180, 200–201, 445–447, 451, 462–463, 473–474, 499–504, 508 | — | — |
| [sandbox-go/time.go](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/sandbox-go/time.go) | — | 34 | 10–11, 16 | — | — |
| [sandbox-go/types.go](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/sandbox-go/types.go) | 7, 10, 18, 21, 29, 32, 38, 41, 51, 70, 87, 147, 150, 168 | — | 3–5, 89–90, 92, 96–97, 109–111, 115–116, 118–119, 121–122, 126–135, 143, 158–159 | — | — |
| [sandbox-python/README.md](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/sandbox-python/README.md) | 98 | — | 85, 91, 93–95 | — | — |
| [sandbox-python/e2e/pool_process.py](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/sandbox-python/e2e/pool_process.py) | — | — | 14 | — | — |
| [sandbox-python/e2e/proxy.py](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/sandbox-python/e2e/proxy.py) | — | — | 63–64 | — | — |
| [sandbox-python/e2e/run.py](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/sandbox-python/e2e/run.py) | 350 | — | 121, 126, 221, 236, 315–316, 440–442, 503–504 | — | 1 |
| [sandbox-python/src/tiny_sandbox/_api.py](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/sandbox-python/src/tiny_sandbox/_api.py) | — | — | 214–216, 236–237, 252–253 | — | — |
| [sandbox-python/src/tiny_sandbox/_cancel.py](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/sandbox-python/src/tiny_sandbox/_cancel.py) | — | — | 34–35 | — | — |
| [sandbox-python/src/tiny_sandbox/_errors.py](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/sandbox-python/src/tiny_sandbox/_errors.py) | — | — | 63 | — | — |
| [sandbox-python/src/tiny_sandbox/_models.py](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/sandbox-python/src/tiny_sandbox/_models.py) | — | — | 188 | — | — |
| [sandbox-python/src/tiny_sandbox/_transport.py](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/sandbox-python/src/tiny_sandbox/_transport.py) | — | — | 41, 98, 182, 237–238, 251–252 | — | — |
| [sandbox-python/src/tiny_sandbox/e2b.py](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/sandbox-python/src/tiny_sandbox/e2b.py) | — | — | 164–165, 329–330, 351–352, 367–368, 433–434 | — | — |
| [sandbox-python/tests/test_background_commands.py](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/sandbox-python/tests/test_background_commands.py) | — | — | 213–214, 801, 807, 827 | — | — |
| [sandbox-ts/README.md](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/sandbox-ts/README.md) | 87 | 84 | 75, 81–82 | — | — |
| [sandbox-ts/checks/e2b-api.ts](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/sandbox-ts/checks/e2b-api.ts) | — | — | 1 | — | — |
| [sandbox-ts/checks/e2b-background-api.ts](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/sandbox-ts/checks/e2b-background-api.ts) | — | — | 1 | — | 24, 26, 28, 30, 32, 34, 36, 38 |
| [sandbox-ts/checks/public-api.ts](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/sandbox-ts/checks/public-api.ts) | — | — | 1 | — | — |
| [sandbox-ts/e2e/package-smoke.mjs](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/sandbox-ts/e2e/package-smoke.mjs) | — | — | 26 | — | — |
| [sandbox-ts/e2e/proxy.mjs](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/sandbox-ts/e2e/proxy.mjs) | 148 | — | 6, 49, 98, 118, 130, 155 | — | — |
| [sandbox-ts/e2e/run.mjs](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/sandbox-ts/e2e/run.mjs) | — | — | 28–29, 88–89, 225, 239, 598, 653 | — | — |
| [sandbox-ts/src/client.ts](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/sandbox-ts/src/client.ts) | — | — | 24, 41, 48, 86, 109, 164, 169, 178, 197–198, 259, 276 | — | — |
| [sandbox-ts/src/e2b.ts](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/sandbox-ts/src/e2b.ts) | — | — | 1, 24, 27, 77, 154, 249, 260–261, 347–348, 429–430, 465, 477–478 | — | — |
| [sandbox-ts/src/errors.ts](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/sandbox-ts/src/errors.ts) | — | — | 10, 54 | — | — |
| [sandbox-ts/src/http.ts](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/sandbox-ts/src/http.ts) | — | — | 15, 42, 71, 85, 91 | — | — |
| [sandbox-ts/src/sse.ts](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/sandbox-ts/src/sse.ts) | — | — | 4, 43, 100–102, 110, 116 | — | — |
| [sandbox-ts/src/stream.ts](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/sandbox-ts/src/stream.ts) | — | — | 8, 37, 74, 85, 95 | — | — |
| [sandbox-ts/src/types.ts](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/sandbox-ts/src/types.ts) | — | — | 1, 11, 29, 32, 37, 49, 51, 54, 86, 88, 90, 92, 99, 104, 117, 119, 121, 123, 129 | — | — |
| [sandbox-ts/tests/background.test.mjs](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/sandbox-ts/tests/background.test.mjs) | — | — | 1–2, 625 | — | — |

### Scripts

| Pre-cleanup file | Tautology | Stale | Decision Record | Excuse | Directive |
| --- | --- | --- | --- | --- | --- |
| [scripts/check-commit-msg.sh](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/scripts/check-commit-msg.sh) | 2–11 | — | — | — | 1 |
| [scripts/coverage.sh](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/scripts/coverage.sh) | 2–5 | 34–35, 39–40 | 6–9 | — | 1 |
| [scripts/e2b-compatibility/background-guest.py](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/scripts/e2b-compatibility/background-guest.py) | — | — | 27, 59, 66–67, 75 | — | — |
| [scripts/e2b-compatibility/background-types.ts](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/scripts/e2b-compatibility/background-types.ts) | — | — | 1, 23 | — | 24, 26 |
| [scripts/e2b-compatibility/background.py](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/scripts/e2b-compatibility/background.py) | — | — | 40, 543, 608, 779–780, 837 | — | — |
| [scripts/e2b-compatibility/installed.sh](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/scripts/e2b-compatibility/installed.sh) | 2 | — | 22–23, 29 | — | 1 |
| [scripts/e2b-compatibility/local.py](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/scripts/e2b-compatibility/local.py) | — | — | 65–66, 106–107, 146–148, 159–160, 171–172, 215, 218–219 | — | — |
| [scripts/e2b-compatibility/observe.py](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/scripts/e2b-compatibility/observe.py) | — | — | 20–22 | — | — |
| [scripts/e2b-compatibility/run.sh](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/scripts/e2b-compatibility/run.sh) | 2–7 | — | — | — | 1 |
| [scripts/e2b-compatibility/runner.py](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/scripts/e2b-compatibility/runner.py) | — | — | 22–24, 145, 301, 356–357 | — | — |
| [scripts/e2b-compatibility/scenarios.py](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/scripts/e2b-compatibility/scenarios.py) | 29, 133, 147, 166, 186, 207, 242, 264, 285, 305, 323, 345, 358, 399, 437, 439, 485, 506, 536, 556, 574 | — | 14–15, 23, 233–234, 418–419, 443–444, 457–458, 583–584 | — | — |
| [scripts/e2b-compatibility/selfcheck.py](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/scripts/e2b-compatibility/selfcheck.py) | 30, 42, 53 | — | 74, 137–138, 150, 156 | — | 13 |
| [scripts/e2b-compatibility/setup.sh](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/scripts/e2b-compatibility/setup.sh) | 2 | — | — | — | 1 |
| [scripts/e2b-compatibility/suite.ts](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/scripts/e2b-compatibility/suite.ts) | — | 1, 22–23, 28–29, 75–76 | 17, 98–99, 125 | — | — |
| [scripts/e2b-compatibility/worker.py](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/scripts/e2b-compatibility/worker.py) | — | — | 43, 45, 49, 93 | — | — |
| [scripts/e2e-concurrency/run.py](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/scripts/e2e-concurrency/run.py) | 115 | — | 19, 144–145, 194, 241–242, 249, 267–268, 278–279, 318 | — | — |
| [scripts/e2e-concurrency/test_runner.py](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/scripts/e2e-concurrency/test_runner.py) | — | — | 18–19 | — | — |
| [scripts/e2e-files.sh](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/scripts/e2e-files.sh) | — | — | — | — | 1 |
| [scripts/e2e-observability.py](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/scripts/e2e-observability.py) | — | — | 77 | — | — |
| [scripts/e2e-observability.sh](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/scripts/e2e-observability.sh) | — | — | — | — | 1 |
| [scripts/e2e-operations.py](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/scripts/e2e-operations.py) | — | — | 169–170 | — | — |
| [scripts/e2e-python.sh](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/scripts/e2e-python.sh) | 2–3 | — | — | — | 1 |
| [scripts/e2e-stdin.sh](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/scripts/e2e-stdin.sh) | — | — | — | — | 1 |
| [scripts/e2e-typescript.sh](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/scripts/e2e-typescript.sh) | 2 | — | — | — | 1 |
| [scripts/e2e.sh](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/scripts/e2e.sh) | 2, 45, 48, 51, 57, 86, 98, 123, 136, 161, 171, 192, 199, 214, 224, 314, 410–411, 423, 467, 518, 527, 576 | 395–398 | 12–14, 28–29, 61–63, 68–69, 202–204, 233–235, 261–264, 272–275, 293–296, 317–323, 340–343, 351–352, 364–370, 444–445, 451–453, 470–476, 498–500, 529–532, 542–544, 565–566 | — | 1 |
| [scripts/file-e2e/go.go](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/scripts/file-e2e/go.go) | 1 | — | — | — | — |
| [scripts/file-e2e/memory/main.go](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/scripts/file-e2e/memory/main.go) | 1 | — | — | — | — |
| [scripts/install.sh](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/scripts/install.sh) | — | — | — | — | 1 |
| [scripts/latency_baseline.py](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/scripts/latency_baseline.py) | — | — | 24, 46 | — | — |
| [scripts/stdin-e2e/run.py](https://github.com/ruhuang2001/tiny-sandbox/blob/0622cb2c369abe4374684ada7c67191e87780e87/scripts/stdin-e2e/run.py) | — | — | 109 | — | — |
