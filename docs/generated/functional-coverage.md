# cortex functional coverage

Generated from the tested operation manifest. Do not edit by hand.

| Operation | Website | API | CLI | Permission | Schemas | Tests |
| --- | :---: | --- | --- | --- | --- | --- |
| `cortex.agent.cancel` | yes | `POST /api/agent/cancel` | `agent cancel` | `session` | `cortex.agent.cancel.request.v1 → cortex.agent.cancel.response.v1` | internal/app/agent_outcome_test.go |
| `cortex.agent.diagnostics` | yes | `GET /api/agent/run-diagnostics` | `agent diagnostics` | `session` | `— → cortex.agent.diagnostics.response.v1` | internal/app/agent_outcome_test.go |
| `cortex.agent.image` | yes | `GET /api/agent/image` | `agent image` | `session` | `— → cortex.agent.image.response.v1` | internal/app/app_test.go |
| `cortex.agent.models` | yes | `GET /api/agent/models` | `agent models` | `session` | `— → cortex.agent.models.response.v1` | internal/app/app_test.go |
| `cortex.agent.run` | yes | `POST /api/agent/run` | `agent run` | `session` | `cortex.agent.run.request.v1 → cortex.agent.run.response.v1` | internal/app/agent_outcome_test.go |
| `cortex.agent.status` | yes | `GET /api/agent/status` | `agent status` | `session` | `— → cortex.agent.status.response.v1` | internal/app/agent_lifecycle_test.go |
| `cortex.auth.google.update` | yes | `POST /api/auth/google` | `google-auth update` | `session` | `cortex.auth.google.update.request.v1 → cortex.auth.google.update.response.v1` | internal/app/auth_hardening_test.go |
| `cortex.auth.login` | yes | `POST /api/auth/login` | `auth login` | `public` | `cortex.auth.login.request.v1 → cortex.auth.login.response.v1` | internal/app/auth_hardening_test.go |
| `cortex.auth.logout` | yes | `POST /api/auth/logout` | `auth logout` | `session` | `cortex.auth.logout.request.v1 → cortex.auth.logout.response.v1` | internal/app/auth_hardening_test.go |
| `cortex.auth.password.update` | yes | `POST /api/auth/password` | `auth-password update` | `session` | `cortex.auth.password.update.request.v1 → cortex.auth.password.update.response.v1` | internal/app/auth_hardening_test.go |
| `cortex.auth.setup` | yes | `POST /api/auth/setup` | `auth setup` | `public` | `cortex.auth.setup.request.v1 → cortex.auth.setup.response.v1` | internal/app/auth_hardening_test.go |
| `cortex.auth.state` | yes | `GET /api/auth/state` | `auth state` | `public` | `— → cortex.auth.state.response.v1` | internal/app/auth_hardening_test.go |
| `cortex.auth.totp.begin` | yes | `POST /api/auth/totp/begin` | `totp begin` | `session` | `cortex.auth.totp.begin.request.v1 → cortex.auth.totp.begin.response.v1` | internal/app/auth_hardening_test.go |
| `cortex.auth.totp.disable` | yes | `POST /api/auth/totp/disable` | `totp disable` | `session` | `cortex.auth.totp.disable.request.v1 → cortex.auth.totp.disable.response.v1` | internal/app/auth_hardening_test.go |
| `cortex.auth.totp.enable` | yes | `POST /api/auth/totp/enable` | `totp enable` | `session` | `cortex.auth.totp.enable.request.v1 → cortex.auth.totp.enable.response.v1` | internal/app/auth_hardening_test.go |
| `cortex.conversation.delete` | yes | `DELETE /api/conversation` | `conversation delete` | `session` | `cortex.conversation.delete.request.v1 → cortex.conversation.delete.response.v1` | internal/app/conversation_integrity_test.go |
| `cortex.conversation.update` | yes | `PUT /api/conversation` | `conversation update` | `session` | `cortex.conversation.update.request.v1 → cortex.conversation.update.response.v1` | internal/app/conversation_integrity_test.go |
| `cortex.conversations.list` | yes | `GET /api/conversations` | `conversations list` | `session` | `— → cortex.conversations.list.response.v1` | internal/app/conversations_test.go |
| `cortex.file.read` | yes | `GET /api/file` | `file get` | `session` | `— → cortex.file.read.response.v1` | internal/app/workspace_hardening_test.go |
| `cortex.files.list` | yes | `GET /api/files` | `files list` | `session` | `— → cortex.files.list.response.v1` | internal/app/workspace_hardening_test.go |
| `cortex.health.read` | no | `GET /api/health` | `health get` | `public` | `— → cortex.health.read.response.v1` | internal/app/routes_test.go |
| `cortex.launcher.config.update` | yes | `PUT /api/launcher/config` | `launcher-config update` | `session` | `cortex.launcher.config.update.request.v1 → cortex.launcher.config.update.response.v1` | internal/app/launcher_test.go |
| `cortex.launcher.instances.list` | yes | `GET /api/launcher/instances` | `launcher-instances list` | `public` | `— → cortex.launcher.instances.list.response.v1` | internal/app/launcher_test.go |
| `cortex.oauth.google.callback` | no | `GET /api/auth/google/callback` | `—` | `public` | `— → cortex.oauth.google.callback.response.v1` | internal/app/auth_hardening_test.go |
| `cortex.oauth.google.start` | yes | `GET /api/auth/google/start` | `—` | `public` | `— → cortex.oauth.google.start.response.v1` | internal/app/auth_hardening_test.go |
| `cortex.roles.list` | yes | `GET /api/manage/roles` | `roles list` | `session` | `— → cortex.roles.list.response.v1` | internal/app/accounts_test.go |
| `cortex.settings.read` | yes | `GET /api/settings` | `settings get` | `session` | `— → cortex.settings.read.response.v1` | internal/app/app_test.go |
| `cortex.settings.update` | yes | `POST /api/settings` | `settings update` | `session` | `cortex.settings.update.request.v1 → cortex.settings.update.response.v1` | internal/app/app_test.go |
| `cortex.status.read` | yes | `GET /api/status` | `status get` | `session` | `— → cortex.status.read.response.v1` | internal/app/app_test.go |
| `cortex.users.list` | yes | `GET /api/manage/users` | `users list` | `session` | `— → cortex.users.list.response.v1` | internal/app/accounts_test.go |
| `cortex.users.manage` | yes | `POST /api/manage/users` | `users manage` | `session` | `cortex.users.manage.request.v1 → cortex.users.manage.response.v1` | internal/app/accounts_test.go |
