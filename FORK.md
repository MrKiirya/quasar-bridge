# Форк

Форк [skrashevich/quasar-bridge](https://github.com/skrashevich/quasar-bridge). Ветка `main` — upstream v0.8.0 плюс доработки ниже;
их описание — в [docs/fork.md](docs/fork.md), остальное — в [wiki upstream](https://github.com/skrashevich/quasar-bridge/wiki).

| Отличие от upstream | Источник | Статус в upstream |
| --- | --- | --- |
| Перекраска анимации кольца по маршруту (`route_indicator`), режимы sysfs и framebuffer | [#3](https://github.com/MrKiirya/quasar-bridge/issues/3) | не предлагалось |
| Разговор с нейросетью без повторного обращения (`llm_conversation`) | [#7](https://github.com/MrKiirya/quasar-bridge/issues/7) | не предлагалось |
| Версия сборки `<версия upstream>+mrkiirya.<N>`, N — номер запуска Actions; самообновление upstream такую версию не трогает | форк | — |

Снято при переходе на v0.8.0: системные промпты из файлов (в upstream — `llm_system_prompt`) и маршрут по слову
station-wakeword через опрос `:8780` (в upstream — `routing.rules[].match.wake_words`, нужен station-wakeword 0.4+
с `tag_alice_wakeword`).

Сборка архива для колонки: Actions → release → Run workflow, артефакт `speaker-release-arm64`.
