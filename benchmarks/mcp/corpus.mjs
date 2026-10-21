const sentences = [
  "Оператор сверяет журнал смены с карточкой объекта и фиксирует причину каждого отклонения.",
  "The service records a request before dispatch, so a restart never loses the original request identifier.",
  "Приёмка считается завершённой после проверки владельца, срока хранения и ссылки на первичный документ.",
  "A delayed response remains visible in the queue until the assigned team confirms the next action.",
  "Для повторного запуска используют сохранённый номер операции; новую запись задним числом не создают.",
  "The weekly review compares the observed value with the approved limit and names the accountable role.",
  "Если источник недоступен, дежурный отмечает время проверки и сообщает следующему ответственному.",
  "Changes are introduced in a small batch, then checked against the previous stable measurement.",
  "Архивная инструкция пригодна только для разбора старых случаев и не задаёт текущий порядок работы.",
  "A routine inspection records the sampled interval, the observed value and the operator who completed the check.",
  "Перед закрытием заявки сохраняют результат контроля, дату и имя проверившего специалиста.",
  "The dashboard groups related events by service key while preserving each event's original timestamp.",
];

function filler(seed, wanted) {
  let out = "";
  let i = 0;
  while (out.length < wanted) {
    const a = sentences[(seed + i * 5) % sentences.length];
    const b = sentences[(seed * 3 + i * 7 + 2) % sentences.length];
    out += `${a} ${b} В контрольной записи ${seed}-${String(i + 1).padStart(3, "0")} сохранены измерение, исполнитель и краткое основание.\n\n`;
    i++;
  }
  return out.slice(0, wanted);
}

function seedOf(value) {
  let hash = 2166136261;
  for (const char of value) hash = Math.imul(hash ^ char.charCodeAt(0), 16777619);
  return hash >>> 0;
}

function article(key, spaceKey, title, summary, tags, sections, target = 0) {
  let body = sections.map(([heading, text]) => `## ${heading}\n\n${text}`).join("\n\n");
  if (target > body.length) body += `${body.endsWith("\n") ? "" : "\n\n"}## Рабочие заметки\n\n${filler(seedOf(`${key}:${spaceKey}`), target - body.length - 22)}`;
  return { key, spaceKey, title, summary, body, tags, properties: { source: "synthetic", status: "frozen" } };
}

function singleSectionArticle(key, spaceKey, title, summary, tags, heading, opening, target) {
  const prefix = `## ${heading}\n\n`;
  const seed = seedOf(`${key}:${spaceKey}`);
  const body = `${prefix}${opening}${filler(seed, target - prefix.length - opening.length)}`.slice(0, target);
  return { key, spaceKey, title, summary, body, tags, properties: { source: "synthetic", status: "frozen" } };
}

export function makeCorpus() {
  const spaces = [
    { key: "ops", name: "Операции / Operations" },
    { key: "platform", name: "Платформа / Platform" },
    { key: "people", name: "Поддержка и люди / Support" },
  ];
  const documents = [
    article("offers-backend", "ops", "offers-backend: разбор инцидента", "Инцидент очереди предложений, симптомы, подтверждённая причина и восстановление.", ["offers-backend", "incident", "очередь"], [
      ["Симптомы", "В 09:14 очередь offers-backend перестала подтверждать новые предложения, хотя API отвечал 200. Рост задержки виден в панели «Операции»; повторная отправка создавала дубликаты."],
      ["Причина и восстановление", "Причиной оказался исчерпанный пул соединений после плановой ротации сертификата. Дежурный остановил повторные отправки, освободил пул и проверил три контрольных предложения. Уникальный факт инцидента: контрольный пакет восстановлен в 09:37 по идентификатору OFR-7319."],
    ], 1450),
    article("offers-current", "ops", "Текущая процедура обработки офферов", "Действующий порядок приёма и сверки предложений.", ["offers", "current", "процедура"], [
      ["Текущий порядок", "Действует с 2026-08-01. Подтверждение текущего порядка: CURRENT-ACK-2026-08. Сначала проверить ключ предложения, затем подтвердить запись в очереди и только после этого уведомить владельца. При повторе использовать исходный idempotency key."],
      ["Эскалация", "Если подтверждения нет пять минут, приложить request id и передать сменному инженеру. Не отправлять дубль вручную."],
    ], 12000),
    article("offers-legacy", "ops", "Устаревшая процедура: ручной импорт офферов", "Архивный порядок до перехода на подтверждение очереди.", ["offers", "obsolete", "архив"], [
      ["Архивный порядок — НЕ ИСПОЛЬЗОВАТЬ", "Устарело с 2026-08-01. Эта процедура описывает ручной импорт из CSV и сохранена только для разбора операций до перехода. Она не является текущей инструкцией."],
      ["Историческая заметка", "Ранее оператор сверял число строк и запускал импорт вручную. Для текущих случаев следовать документу «Текущая процедура обработки офферов»."],
    ], 2800),
    article("queue-watch", "ops", "Наблюдение за очередью", "Пороговые значения и передача смены.", ["queue", "monitoring"], [["Порог и действие", "Проверять возраст старейшей записи каждые десять минут. При превышении пяти минут сохранить снимок панели и передать ключ очереди ответственному инженеру. Уникальный малый контрольный факт: QUEUE-ACK-520-SMALL."]], 520),
    article("shipment-handoff", "ops", "Передача смены по отправкам", "Как передать незакрытые отправки следующему оператору.", ["handoff", "shipment"], [["Передача", "В карточке смены укажите номер отправки, последнюю подтверждённую операцию и ожидаемый ответ партнёра. Получатель подтверждает приём до закрытия смены."]], 2100),
    article("returns-guide", "ops", "Возвраты: проверка и решение", "Проверка возврата по квитанции и состоянию упаковки.", ["returns", "service"], [["Проверка", "Сопоставьте номер квитанции с записью заказа. Если упаковка повреждена, приложите фотографию и назначьте ручную проверку; автоматическое одобрение в этом случае запрещено."]], 3300),
    article("dispatch-map", "ops", "Справочник маршрутизации", "Внутренние названия команд и их зоны ответственности.", ["routing", "directory"], [["Команды", "Команда «Север» принимает срочные отправки, «Центр» разбирает расхождения адреса, а «Юг» подтверждает возвраты. Название команды не заменяет идентификатор сервиса."]], 1900),
    article("platform-api", "platform", "Service API: request lifecycle", "Контракт создания запроса и обработки повторов.", ["service-api", "API", "idempotency"], [["Request lifecycle", "The caller supplies a stable request key. The gateway stores the key before dispatch and returns the original result for a retry. A changed payload with the same key receives a conflict response."]], 3400),
    article("svc-orbit", "platform", "Orbit: service ownership", "Каталог владельцев сервиса Orbit и его сокращённого имени.", ["Orbit", "alias", "service"], [["Identity", "Orbit is the customer-facing name of the settlement service. Its short alias is ORB; the internal deployment label is orbit-ledger. Owners: Platform Ledger, escalation channel ledger-oncall."]], 2500),
    article("orbit-tools", "people", "Orbit tools onboarding", "Внутренний набор инструментов команды Orbit.", ["Orbit", "onboarding", "tools"], [["Team context", "Orbit is also the nickname of the Support team's internal toolbox. It is not the settlement service and has no ledger deployment. Ask the support coordinator for access."]], 2200),
    article("deploy-current", "platform", "Current deployment procedure", "Текущий порядок выкладки сервиса.", ["deploy", "current", "release"], [["Before rollout", "Confirm the build digest and the change owner. Start with one canary instance, compare error rate for ten minutes, then continue in batches only when the health check remains green."]], 3100),
    article("deploy-archive", "platform", "Obsolete deployment procedure (archive)", "Старый порядок выкладки, оставленный для истории.", ["deploy", "obsolete", "archive"], [["Historical procedure — obsolete", "Do not use for current releases. This archive describes a former all-at-once restart. The approved current process is the canary rollout in Current deployment procedure."]], 1700),
    article("cache-runbook", "platform", "Cache recovery runbook", "Восстановление кеша после роста задержки.", ["cache", "recovery"], [["Recovery", "Inspect eviction rate and key age first. Warm the read-through cache in bounded batches, then compare hit ratio with the previous hourly baseline. Never purge the shared namespace during peak traffic."]], 3600),
    article("ledger-reconcile", "platform", "Ledger reconciliation", "Сверка проводок между журналом и итоговой ведомостью.", ["ledger", "reconciliation"], [["Daily check", "Match the settlement batch id on both sides and compare debit and credit totals. A mismatch stays open until the source event and correction are attached to the same case."]], 4700),
    article("token-rotation", "platform", "Credential rotation schedule", "Сроки и проверка ротации машинных ключей.", ["credential", "rotation", "security"], [["Schedule", "Rotate service credentials every ninety days or immediately after suspected exposure. Verify the new credential in a canary request before revoking the previous one."]], 2300),
    article("api-errors", "platform", "API error catalogue", "Коды ошибок и действия вызывающей стороны.", ["API", "errors", "reference"], [["Conflict response", "HTTP 409 means the same idempotency key arrived with a different payload. Preserve the response body and open a review; do not retry with a new key until the caller confirms intent."]], 2800),
    article("telemetry-fields", "platform", "Telemetry field guide", "Значение основных полей событий мониторинга.", ["telemetry", "monitoring"], [["Event fields", "The service_key identifies the emitting service, trace_id joins related spans, and observed_at preserves the source clock. Dashboard display time may use a local timezone."]], 2400),
    article("support-intake", "people", "Support request intake", "Что собрать при регистрации обращения.", ["support", "intake", "request"], [["Required details", "Соберите точное время, язык интерфейса, номер обращения и ожидаемый результат. Не просите пользователя повторять действие, если это может создать платёж или отправку."]], 3800),
    article("account-access", "people", "Account access guide", "Выдача и отзыв доступа к рабочим инструментам.", ["account", "access"], [["Approval", "Укажите роль и срок доступа. Владелец данных подтверждает запрос в системе учёта; после завершения работы доступ отзывается в тот же рабочий день."]], 2600),
    article("language-support", "people", "Языковая поддержка", "Правила передачи обращений на русском и английском.", ["language", "support", "Русский"], [["Передача", "Сохраните исходную формулировку пользователя и добавьте краткое резюме на языке команды. Не переводите идентификаторы, названия сервисов и фрагменты кода."]], 1800),
    article("incident-comms", "people", "Incident communication guide", "Короткие обновления во время инцидента.", ["incident", "communication"], [["Update format", "State what is affected, the last verified observation, the action owner and the next update time. Mark estimates as estimates and keep the incident key in every update."]], 2700),
    article("reimbursement", "people", "Expense reimbursement", "Подготовка заявки на возмещение расходов.", ["expense", "reimbursement"], [["Submission", "Attach a dated receipt, choose the cost centre and describe the business purpose in one sentence. The approver returns incomplete requests with a specific missing field."]], 3200),
    article("oncall-glossary", "people", "On-call glossary", "Термины для дежурств и эскалаций.", ["on-call", "glossary"], [["Terms", "A primary is the first responder, a secondary is the backup, and an incident commander coordinates decisions. Escalation transfers responsibility only after acknowledgement."]], 2300),
    article("handover-template", "people", "Handover note template", "Шаблон передачи открытой работы.", ["handover", "template"], [["Template", "Include current state, evidence link, next action, owner and deadline. Separate confirmed facts from assumptions so the receiving person can continue without repeating checks."]], 2200),
    article("release-evidence", "platform", "Release evidence checklist", "Артефакты для подтверждения безопасного релиза.", ["release", "evidence"], [["Evidence", "Record build digest, canary start time, health observations and approver. Link the change request with [[link:deploy-current]] so readers can open the current deployment procedure."]], 2100),
    article("load-profile", "platform", "Load profile reference", "Профили нагрузки для планирования ёмкости.", ["load", "capacity"], [["Profile notes", "The profile separates steady traffic from short bursts. Compare request count, payload class and queue age; aggregate throughput alone can hide a slow large request."], ["Capacity observations", "Unique large-section answer: LOAD-6P2X-Q91D. " + filler(41, 2600)]], 90000),
    singleSectionArticle("audit-event", "platform", "Audit event payload", "Пример большого события для проверки чтения одной секции.", ["audit", "event", "payload"], "Payload record", "Unique audit marker: AUDIT-7Q4M-91ZX. ", 70000),
  ];

  // Two same-title sections make the read anchor's 1-based occurrence meaningful.
  documents.push(article("support-escalation", "people", "Support escalation and duplicate heading", "Эскалация обращения с повторяющимся заголовком.", ["support", "escalation"], [
    ["Escalation", "Первичная проверка фиксирует время, владельца и номер обращения. При отсутствии ответа назначьте резервного специалиста."],
    ["Evidence", "Передайте снимок ошибки и последнюю успешную операцию в закрытый канал команды."],
    ["Escalation", "Повторный раздел описывает передачу уже принятого инцидента. Получатель подтверждает ответственность и сообщает следующий срок обновления. Уникальный ответ: эскалацию приняла группа TIER-2-RU."],
  ], 12500));

  // Gold labels are authored judgments for this frozen fixture, not exhaustive production recall labels.
  const searchTasks = [
    { key: "s01", category: "service", query: "offers-backend incident queue stopped acknowledging", relevantArticleKeys: ["offers-backend"] },
    { key: "s02", category: "alias", query: "ORB owner and escalation channel", relevantArticleKeys: ["svc-orbit"] },
    { key: "s03", category: "paraphrase", query: "как восстановить зависшие предложения без повторной отправки", relevantArticleKeys: ["offers-backend", "offers-current"] },
    { key: "s04", category: "morphology", query: "проверки возвратов и повреждённой упаковки", relevantArticleKeys: ["returns-guide"] },
    { key: "s05", category: "body-only", query: "idempotency key returns original result", relevantArticleKeys: ["platform-api"] },
    { key: "s06", category: "code", query: "HTTP 409 same key different payload", relevantArticleKeys: ["api-errors"] },
    { key: "s07", category: "false-friend", query: "Orbit settlement ledger deployment label", relevantArticleKeys: ["svc-orbit"] },
    { key: "s08", category: "missing term", query: "протокол лунного маяка ZK-884", relevantArticleKeys: [] },
    { key: "s09", category: "crossspace", query: "что передать при передаче незакрытой работы получателю", relevantArticleKeys: ["shipment-handoff", "handover-template"] },
    { key: "s10", category: "service", query: "cache recovery shared namespace peak traffic", relevantArticleKeys: ["cache-runbook"] },
    { key: "s11", category: "paraphrase", query: "кто принимает незакрытую сменную работу и что передать", relevantArticleKeys: ["handover-template", "shipment-handoff"] },
    { key: "s12", category: "body-only", query: "OFR-7319 09:37", relevantArticleKeys: ["offers-backend"] },
  ];

  const readTasks = [
    { key: "r01", query: "найти уникальный код восстановленного контрольного пакета", articleKey: "offers-backend", headingTitle: "Причина и восстановление", headingOccurrence: 1, answer: "OFR-7319" },
    { key: "r02", query: "В действующей процедуре обработки офферов найдите буквальную отметку подтверждения", articleKey: "offers-current", headingTitle: "Текущий порядок", headingOccurrence: 1, answer: "CURRENT-ACK-2026-08" },
    { key: "r03", query: "какую группу приняла эскалацию во втором разделе", articleKey: "support-escalation", headingTitle: "Escalation", headingOccurrence: 2, answer: "TIER-2-RU" },
    { key: "r04", query: "найти маркер большого audit payload", articleKey: "audit-event", headingTitle: "Payload record", headingOccurrence: 1, answer: "AUDIT-7Q4M-91ZX" },
    { key: "r05", query: "найти поле связи событий", articleKey: "telemetry-fields", headingTitle: "Event fields", headingOccurrence: 1, answer: "trace_id" },
    { key: "r06", query: "найти уникальный ответ в крупном разделе профиля нагрузки", articleKey: "load-profile", headingTitle: "Capacity observations", headingOccurrence: 1, answer: "LOAD-6P2X-Q91D" },
    { key: "r07", query: "найти контрольный факт в короткой статье о наблюдении за очередью", articleKey: "queue-watch", headingTitle: "Порог и действие", headingOccurrence: 1, answer: "QUEUE-ACK-520-SMALL" },
  ];
  const allBodies = documents.map((d) => d.body).join("\n");
  for (const task of readTasks) {
    const occurrences = allBodies.split(task.answer).length - 1;
    if (occurrences !== 1) throw new Error(`Read answer ${task.key} occurs ${occurrences} times across document bodies`);
    const metadata = documents.map((d) => [d.title, d.summary, ...d.tags, ...[...d.body.matchAll(/^## (.+)$/gm)].map((m) => m[1])].join(" ")).join("\n");
    if (metadata.includes(task.answer)) throw new Error(`Read answer ${task.key} appears in document metadata or a heading`);
  }
  return { version: "mcp-baseline-v1", spaces, documents, searchTasks, readTasks };
}
