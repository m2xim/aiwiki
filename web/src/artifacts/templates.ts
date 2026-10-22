// Starter files for new artifacts.
// Templates use the AI Wiki kit (docs/ARTIFACT-KIT.md): the server serves /artifact-runtime/aiwiki-kit.js and .css.
// The kit reports the frame height itself, so templates do not post aiwiki:resize.
import type { DraftFile } from '../artifactsCore';
import { textBytes } from '../artifactsCore';

export type TemplateId = 'empty' | 'table' | 'dashboard';
export type Template = { id: TemplateId; title: string; description: string; files: () => DraftFile[] };

const head = (title: string) => `<!doctype html>
<html lang="ru">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>${title}</title>
<link rel="stylesheet" href="/artifact-runtime/aiwiki-kit.css">
<script src="/artifact-runtime/aiwiki-kit.js"></script>
`;
const EMPTY = `${head('Артефакт')}<style>
body { font-family: system-ui, sans-serif; margin: 0; padding: 20px; color: #222; background: transparent; }
@media (prefers-color-scheme: dark) { body { color: #eee; } }
</style>
</head>
<body>
<h1>Новый артефакт</h1>
<p>Измените этот файл: здесь можно разместить любой HTML, CSS и JavaScript.</p>
</body>
</html>
`;

const TABLE = `${head('Таблица с фильтрами')}</head>
<body>
<h1>Таблица задач</h1>
<p class="aw-muted">Поиск, фильтры по столбцам, сортировка по заголовку, группировка и выгрузка в CSV уже встроены. Замените данные ниже своими.</p>
<div id="tbl"></div>
<script>
  // Данные лежат прямо в файле: сеть в песочнице запрещена.
  const rows = [
    { задача: 'Подготовить отчёт', ответственный: 'Анна', статус: 'В работе', срок: '2026-11-03', часы: 12 },
    { задача: 'Согласовать бюджет', ответственный: 'Борис', статус: 'Готово', срок: '2026-10-20', часы: 7 },
    { задача: 'Обновить сайт', ответственный: 'Анна', статус: 'Новая', срок: '2026-11-15', часы: 30 },
    { задача: 'Провести обучение', ответственный: 'Вера', статус: 'В работе', срок: '2026-11-08', часы: 16 },
    { задача: 'Проверить договоры', ответственный: 'Борис', статус: 'Новая', срок: '2026-12-01', часы: 9 },
    { задача: 'Запустить рассылку', ответственный: 'Вера', статус: 'Готово', срок: '2026-10-25', часы: 5 }
  ];
  AIWiki.table('#tbl', {
    title: 'Задачи',
    rows: rows,
    groupBy: 'статус',
    totals: true,
    csvName: 'zadachi',
    columns: [
      { key: 'задача', label: 'Задача' },
      { key: 'ответственный', label: 'Ответственный', filter: 'select' },
      { key: 'статус', label: 'Статус', filter: 'select' },
      { key: 'срок', label: 'Срок', type: 'date' },
      { key: 'часы', label: 'Часы', type: 'number', format: 'integer', agg: 'sum' }
    ],
    highlight: function (r) { return r.статус === 'Готово' ? 'good' : false; }
  });
</script>
</body>
</html>
`;

const DASHBOARD = `${head('Дашборд с графиком')}</head>
<body>
<h1>Продажи по месяцам</h1>
<p class="aw-muted">Двигайте ползунок: показатели и график пересчитываются сами. Замените данные ниже своими.</p>
<aiwiki-slider id="months" label="Месяцев на графике" min="3" max="12" step="1" value="12"></aiwiki-slider>
<div id="kpi"></div>
<div id="chart"></div>
<script>
  // Данные лежат прямо в файле: сеть в песочнице запрещена.
  const names = ['янв', 'фев', 'мар', 'апр', 'май', 'июн', 'июл', 'авг', 'сен', 'окт', 'ноя', 'дек'];
  const revenue = [120000, 135000, 128000, 160000, 175000, 168000, 190000, 185000, 210000, 205000, 230000, 250000];

  const s = AIWiki.state({ months: 12 });
  AIWiki.bind('#months', s, 'months');

  const kpi = AIWiki.stats('#kpi', [
    { label: 'Выручка', value: 0, format: 'currency', digits: 0 },
    { label: 'Лучший месяц', value: 0, format: 'currency', digits: 0 },
    { label: 'Средняя за месяц', value: 0, format: 'currency', digits: 0 }
  ]);
  const chart = AIWiki.lineChart('#chart', { title: 'Выручка по месяцам', labels: names, series: { 'Выручка': revenue }, format: 'currency', digits: 0, fill: true });

  AIWiki.effect(function () {
    const v = revenue.slice(0, s.months);
    const sum = v.reduce(function (a, b) { return a + b; }, 0);
    kpi[0].update({ value: sum });
    kpi[1].update({ value: Math.max.apply(null, v) });
    kpi[2].update({ value: Math.round(sum / v.length) });
    chart.update({ labels: names.slice(0, s.months), series: { 'Выручка': v } });
  });
</script>
</body>
</html>
`;

const file = (path: string, content: string): DraftFile => ({ path, encoding: 'utf8', content, original: null, size: textBytes(content) });

export const TEMPLATES: Template[] = [
  { id: 'empty', title: 'Пустой', description: 'Страница с заголовком: начните с чистого листа.', files: () => [file('index.html', EMPTY)] },
  { id: 'table', title: 'Таблица с фильтрами', description: 'Таблица с поиском, фильтром по статусу и сортировкой по столбцам.', files: () => [file('index.html', TABLE)] },
  { id: 'dashboard', title: 'Дашборд с графиком и ползунком', description: 'Показатели и столбчатый график, который меняется ползунком.', files: () => [file('index.html', DASHBOARD)] },
];
