# Форк

Форк [skrashevich/quasar-bridge](https://github.com/skrashevich/quasar-bridge).

| Отличие от upstream | Источник | Статус в upstream |
| --- | --- | --- |
| Системные промпты из файлов (`prompts.assistant_file`, `prompts.retry_file`) | [PR #5](https://github.com/skrashevich/quasar-bridge/pull/5), @xeezh | не принят |
| Перекраска анимации кольца по маршруту (`route_indicator`) | [#3](https://github.com/MrKiirya/quasar-bridge/issues/3) | не предлагалось |
| Версия сборки `<версия upstream>+mrkiirya.<N>`, N — номер запуска Actions (`build-yap.py --suffix`) | форк | — |

Сборка `.yap`: Actions → release → Run workflow, артефакт `yap-release`.
