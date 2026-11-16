# CLAUDE.md

Project rules for AI assistants and contributors working on dental-server.

## Project Consistency & English-Only Codebase

### Project Architecture and Coding Style
- The entire project must follow a unified and consistent architecture.
- Maintain the same folder structure, naming conventions, coding patterns, and architectural principles across all modules.
- Follow the existing coding style throughout the project.
- Before implementing anything, inspect existing implementations and follow their conventions.
- Do not introduce alternative patterns or inconsistent approaches without explicit approval.
- Reuse existing components, utilities, services, and shared logic whenever possible.

### Dependency Installation Approval
- Always request explicit approval before installing any new package, library, framework, or development tool.
- Never install, upgrade, replace, or remove dependencies without permission.
- Before requesting approval, explain why the new dependency is necessary and why existing project resources are insufficient.
- Always prioritize existing libraries and built-in project capabilities.

### English-Only Project Content
**All written content inside the project must be in English.**

This rule applies to:
- Source code comments.
- Variable, function, class, and file names.
- UI labels and descriptions.
- Error messages and validation messages.
- Logs and debugging messages.
- Documentation and README files.
- Commit messages and other developer-facing text.
- Any other written content added to the codebase.

Do not introduce Persian or any other non-English language directly into source code, comments, documentation, or project interfaces.

### Localization
- The project's source code, documentation, and developer-facing content must remain English-only.
- The application must still support all languages configured in its existing localization system.
- User-facing translations are stored only in `internal/languages/files/*.json` (see `internal/languages/README.md`).
- Whenever a new translation is required, provide an equivalent entry for every currently supported language file in that folder.
- Follow the existing localization architecture.

### Final Rule
One unified architecture, one consistent coding style, English-only source and documentation, approval before every new dependency, and complete multilingual support through the existing localization system.
