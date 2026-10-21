export function makeSectionQueries() {
  return {
    version: "section-queries-v2",
    queries: [
      {
        key: "n01",
        category: "article-identity-heading",
        query: "offers-backend incident symptom after certificate rotation",
        relevantSections: [
          { articleKey: "offers-backend", headingTitle: "Симптомы", headingOccurrence: 1 },
        ],
        rationale: "The article identity and the symptom clue point to the incident's symptom section; the certificate rotation is described as the cause in a separate section, which is a close distractor.",
      },
      {
        key: "n02",
        category: "metadata-only",
        query: "deploy archive obsolete",
        relevantSections: [
          { articleKey: "deploy-archive", headingTitle: "Historical procedure — obsolete", headingOccurrence: 1 },
        ],
        rationale: "These terms identify the archived deployment article through its title and tags; its primary substantive section is the historical procedure. The current deployment article is a near distractor.",
      },
      {
        key: "n03",
        category: "morphology-ru",
        query: "проверить возврат после повреждения упаковки",
        relevantSections: [
          { articleKey: "returns-guide", headingTitle: "Проверка", headingOccurrence: 1 },
        ],
        rationale: "The inflected action and damaged-package clue paraphrase the return inspection section, whose body specifies the manual check.",
      },
      {
        key: "n04",
        category: "english-russian-natural-question",
        query: "Which Orbit is the расчётный сервис, and what is its short alias?",
        relevantSections: [
          { articleKey: "svc-orbit", headingTitle: "Identity", headingOccurrence: 1 },
        ],
        rationale: "The settlement-service identity and short alias are defined in this section. The Support team's Orbit toolbox is an explicit close distractor, but it is not a settlement service.",
      },
      {
        key: "n05",
        category: "close-distractors-retry-contract",
        query: "same request key reused with changed payload: retry outcome",
        relevantSections: [
          { articleKey: "platform-api", headingTitle: "Request lifecycle", headingOccurrence: 1 },
          { articleKey: "api-errors", headingTitle: "Conflict response", headingOccurrence: 1 },
        ],
        rationale: "Both sections explicitly describe the changed-payload/same-key conflict: one defines the lifecycle behavior and the other explains the HTTP response. Both are defensible section-level gold judgments.",
      },
      {
        key: "n06",
        category: "code-identifier",
        query: "service_key identifies the emitting service",
        relevantSections: [
          { articleKey: "telemetry-fields", headingTitle: "Event fields", headingOccurrence: 1 },
        ],
        rationale: "The code identifier and its meaning appear in the telemetry field definitions.",
      },
      {
        key: "n07",
        category: "repeated-heading-occurrence",
        query: "Support escalation once an incident is accepted; receiver confirms ownership",
        relevantSections: [
          { articleKey: "support-escalation", headingTitle: "Escalation", headingOccurrence: 2 },
        ],
        rationale: "The accepted-incident handoff and receiver acknowledgement are in the second of two identically titled sections. The first section describes initial triage and backup assignment.",
      },
      {
        key: "n08",
        category: "verbose-natural-question",
        query: "Where should an operator look before warming the read-through cache, and which comparison shows that recovery worked?",
        relevantSections: [
          { articleKey: "cache-runbook", headingTitle: "Recovery", headingOccurrence: 1 },
        ],
        rationale: "The section gives the pre-warm inspection and the post-warm comparison, making both parts of the question answerable there.",
      },
      {
        key: "n09",
        category: "short-phrase",
        query: "canary error rate ten minutes",
        relevantSections: [
          { articleKey: "deploy-current", headingTitle: "Before rollout", headingOccurrence: 1 },
        ],
        rationale: "The canary and ten-minute error-rate observation are specified together in the current rollout section.",
      },
      {
        key: "n10",
        category: "short-phrase",
        query: "handover evidence owner deadline",
        relevantSections: [
          { articleKey: "handover-template", headingTitle: "Template", headingOccurrence: 1 },
        ],
        rationale: "The requested handover fields are enumerated in the template section.",
      },
      {
        key: "n11",
        category: "explicit-historical-intent",
        query: "For historical cases before queue confirmation, which manual CSV import procedure was used?",
        relevantSections: [
          { articleKey: "offers-legacy", headingTitle: "Архивный порядок — НЕ ИСПОЛЬЗОВАТЬ", headingOccurrence: 1 },
          { articleKey: "offers-legacy", headingTitle: "Историческая заметка", headingOccurrence: 1 },
        ],
        rationale: "Both archival sections are valid for an explicitly historical request: the first describes the former CSV procedure and the second confirms the prior manual import. The current offers procedure is a close distractor and is excluded by the historical wording.",
      },
      {
        key: "n12",
        category: "natural-question-ru",
        query: "Передать незакрытую отправку следующему оператору: что указать и кто подтверждает приём?",
        relevantSections: [
          { articleKey: "shipment-handoff", headingTitle: "Передача", headingOccurrence: 1 },
        ],
        rationale: "Both the requested shipment details and the receiving operator's acknowledgement are in the shipment handoff section.",
      },
      {
        key: "n13",
        category: "close-distractor-entity-disambiguation",
        query: "Orbit Support toolbox onboarding access coordinator",
        relevantSections: [
          { articleKey: "orbit-tools", headingTitle: "Team context", headingOccurrence: 1 },
        ],
        rationale: "This identifies the Support team's internal Orbit toolbox and access route. The settlement service with the same name is the deliberate distractor.",
      },
      {
        key: "n14",
        category: "article-identity-heading",
        query: "ledger reconciliation daily settlement batch debit credit totals",
        relevantSections: [
          { articleKey: "ledger-reconcile", headingTitle: "Daily check", headingOccurrence: 1 },
        ],
        rationale: "The article identity and all reconciliation clues map to its daily check section.",
      },
      {
        key: "n15",
        category: "morphology-en",
        query: "reimbursing expenses with dated receipt and cost centre approval",
        relevantSections: [
          { articleKey: "reimbursement", headingTitle: "Submission", headingOccurrence: 1 },
        ],
        rationale: "The reimbursement wording is a morphological variant of the article topic; receipt, cost centre, and approver are covered in Submission.",
      },
      {
        key: "n16",
        category: "natural-question-ru",
        query: "Что собрать при регистрации обращения, если нужно передать язык интерфейса и ожидаемый результат?",
        relevantSections: [
          { articleKey: "support-intake", headingTitle: "Required details", headingOccurrence: 1 },
        ],
        rationale: "The section lists the intake details named in the question, including interface language and expected outcome.",
      },
      {
        key: "n17",
        category: "morphology-en",
        query: "rotating machine credentials after suspected exposure or every ninety days",
        relevantSections: [
          { articleKey: "token-rotation", headingTitle: "Schedule", headingOccurrence: 1 },
        ],
        rationale: "The schedule section gives both the normal rotation interval and the exposure-triggered rotation rule.",
      },
      {
        key: "n18",
        category: "negative-absent-concept",
        query: "палеонтология аммонитов стратиграфическая колонка",
        relevantSections: [],
        rationale: "The frozen corpus has no paleontology, ammonites, or stratigraphic columns in its article metadata, headings, authored text, or generated filler; the query's content terms have no generic operational overlap.",
      },
      {
        key: "n19",
        category: "negative-absent-concept",
        query: "neutrino oscillation scintillator detector",
        relevantSections: [],
        rationale: "The corpus contains no neutrino physics, scintillators, or particle detectors. These distinctive content terms do not occur in article metadata, headings, authored text, or generated filler.",
      },
      {
        key: "n20",
        category: "negative-absent-concept",
        query: "глазурование фарфора керамическая глазурь",
        relevantSections: [],
        rationale: "Ceramic glazing and porcelain are absent from the frozen corpus, including its repeated generic filler; the content words have no incidental match to its operational material.",
      },
      {
        key: "n21",
        category: "negative-absent-concept",
        query: "exoplanet transit spectroscopy atmospheric retrieval",
        relevantSections: [],
        rationale: "The corpus has no exoplanet observations or atmospheric spectroscopy. The specialized content terms are absent from its metadata, headings, authored text, and filler.",
      },
      {
        key: "n22",
        category: "negative-absent-concept",
        query: "микориза грибница спорообразование",
        relevantSections: [],
        rationale: "Fungal ecology and spore formation are not represented anywhere in the frozen corpus, and these specific terms do not match its generic operational filler.",
      },
    ],
  };
}
