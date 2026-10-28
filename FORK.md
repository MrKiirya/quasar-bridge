# Форк

Форк [skrashevich/quasar-bridge](https://github.com/skrashevich/quasar-bridge).

| Отличие от upstream | Источник | Статус в upstream |
| --- | --- | --- |
| Системные промпты из файлов (`prompts.assistant_file`, `prompts.retry_file`) | [PR #5](https://github.com/skrashevich/quasar-bridge/pull/5), @xeezh | не принят |
| Перекраска анимации кольца по маршруту (`route_indicator`) | [#3](https://github.com/MrKiirya/quasar-bridge/issues/3) | не предлагалось |
| Маршрут по своему слову активации station-wakeword (`wake_word`, `wake_word_any`) | форк | не предлагалось |
| Разговор с нейросетью без повторного обращения (`llm_conversation`) | [#7](https://github.com/MrKiirya/quasar-bridge/issues/7) | не предлагалось |
| Версия сборки `<версия upstream>+mrkiirya.<N>`, N — номер запуска Actions (`build-yap.py --suffix`) | форк | — |

Сборка `.yap`: Actions → release → Run workflow, артефакт `yap-release`.

Ссылки на описание каждой доработки в `docs/guide.md` — в начале [README](README.md).
