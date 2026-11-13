# pg-copycheck

[![CI](https://github.com/Santaniel0/pg-copycheck/actions/workflows/ci.yml/badge.svg)](https://github.com/Santaniel0/pg-copycheck/actions/workflows/ci.yml)

Проверяет CSV-файл перед загрузкой в PostgreSQL через `COPY ... FROM` (или `\copy`). Файл читается так же, как его прочитал бы сервер, и за один проход находятся все строки, на которых COPY упадёт. Подключение к базе не нужно: достаточно схемы таблицы из `pg_dump --schema-only`.

## Зачем

Типичная ситуация. Поставщик, 1С или Excel выгрузили `orders.csv` на пару гигабайт, его нужно залить в таблицу `orders`. COPY работает по принципу «всё или ничего» и останавливается на первой ошибке:

```
ERROR:  invalid input syntax for type integer: "12 шт"
CONTEXT:  COPY orders, line 377617, column quantity: "12 шт"
```

Строку исправили, загрузку запустили снова, и через десять минут она падает на строке 912004 с другой ошибкой. Каждая неудачная попытка вдобавок оставляет в таблице мёртвые строки до следующего VACUUM.

В PostgreSQL 17 появился `ON_ERROR ignore`, но он пропускает только ошибки преобразования значения в тип и никуда не сохраняет пропущенные строки. Лишние или недостающие колонки, байты не в UTF-8 (выгрузка из Excel в cp1251), NUL-байты, незакрытая кавычка, NULL в NOT NULL и дубли первичного ключа по-прежнему валят всю загрузку. Обычный обходной путь — staging-таблица из text-колонок и проверки через `pg_input_is_valid`. Он требует доступа к базе, места под полную копию данных и сам ломается на тех же структурных ошибках.

pg-copycheck:

- показывает все проблемы сразу, сгруппированные по колонке и виду ошибки, с текстом, который выдал бы сам PostgreSQL;
- называет строку, на которой COPY остановится первой;
- делит файл на `--clean` (записи, которые COPY примет, байт в байт) и `--rejects` (остальные, с причиной).

## Установка

Готовые сборки для Linux, Windows и macOS лежат на странице [Releases](https://github.com/Santaniel0/pg-copycheck/releases/latest), Go для них не нужен. Собрать самому через Go:

```
go install github.com/Santaniel0/pg-copycheck/cmd/pg-copycheck@latest
```

Или из исходников: `go build ./cmd/pg-copycheck`. Получается один исполняемый файл, внешних зависимостей нет, только стандартная библиотека Go (нужен Go 1.27).

## Пример

В репозитории есть генератор тестовой выгрузки заказов. В каждую тысячную строку он подкладывает типичную ошибку и печатает, что и сколько раз подложил:

```
$ go run ./cmd/genfixture -rows 20000 -fault-every 1000 -seed 42 -o orders.csv -ddl orders.sql
20000 rows, 20 faulty: bad integer 2; value too long 2; null in NOT NULL column 2; extra column 2; numeric overflow 1; unknown enum label 1; day-first timestamp 1; Windows-1251 bytes 1; NUL byte 1; duplicate primary key 1; bad uuid 1; bad json 1; impossible date 1; bad boolean 1; missing column 1; time zone name (valid, but not decided offline) 1;
```

`orders.sql` записан так же, как его выдал бы `pg_dump --schema-only`: enum `order_status`, таблица `orders` с `numeric(12,2)`, `varchar(64)`, `timestamptz`, `uuid`, `jsonb`, генерируемой колонкой, первичным ключом и UNIQUE.

Проверка (вывод настоящий, скопирован из терминала):

```
$ pg-copycheck --ddl orders.sql --header match --check-unique --max-examples 1 \
      --clean orders.clean.csv --rejects orders.rejects.csv orders.csv
input:   orders.csv
table:   public.orders (14 columns from the file)
format:  CSV, DELIMITER ',', QUOTE '"', ESCAPE '"', NULL "", HEADER match; LF line endings
server:  PostgreSQL 17, DateStyle MDY
read:    20000 records after the header line, 3.5 MiB, in 0.02s

ERRORS: COPY would stop at the first of these
   RECORDS  SQLSTATE  COLUMN          PROBLEM
         2  22P02     quantity        invalid input syntax for type integer
                                        line 1126: invalid input syntax for type integer: "12 шт"
         2  22001     email           value too long for type character varying(64)
                                        line 2267
                                          note: value: "very.long.address.that.someone.typed.into.the.wrong.field@ex"...
         2  23502     customer_id     null value in column "customer_id" of relation "orders" violates not-null constraint
                                        line 3383
         2  22P04                     extra data after last expected column
                                        line 4511
         1  22003     amount          numeric field overflow
                                        line 5622
                                          detail: A field with precision 12, scale 2 must round to an absolute value less than 10^10.
                                          note: value: "12345678901.50"
         1  22P02     status          invalid input value for enum order_status
                                        line 6744: invalid input value for enum order_status: "refunded"
         1  22008     created_at      date/time field value out of range
                                        line 7870: date/time field value out of range: "20.03.2024 13:04"
                                          hint: Perhaps you need a different "datestyle" setting.
         2  22021                     invalid byte sequence for encoding "UTF8"
                                        line 8989: invalid byte sequence for encoding "UTF8": 0xc4 0xee
         1  23505                     duplicate key value violates unique constraint "orders_pkey"
                                        line 11256
                                          note: same key as line 11255
         1  22P02     external_id     invalid input syntax for type uuid
                                        line 12389: invalid input syntax for type uuid: "not-a-uuid"
         1  22P02     meta            invalid input syntax for type json
                                        line 13505
                                          note: value: "{\"source\": web}"
         1  22008     shipped_on      date/time field value out of range
                                        line 14618: date/time field value out of range: "2024-02-30"
         1  22P02     paid            invalid input syntax for type boolean
                                        line 15750: invalid input syntax for type boolean: "да"
         1  22P04     comment         missing data for column "comment"
                                        line 16872
  The invalid bytes look like Windows-1251 text. If the whole file is in that
  encoding, COPY ... (FORMAT csv, ENCODING 'WIN1251') converts it on load.

WARNINGS: not decided offline or worth a look
   RECORDS  SQLSTATE  COLUMN          PROBLEM
         1            created_at      timestamp with time zone value outside the forms checked offline
                                        line 17996: "2024-04-20 02:03:00 MSK"

UNIQUE KEYS (rows already in the table are not known)
  orders_pkey (id): checked within the file
  orders_external_id_key (external_id): checked within the file

RESULT: COPY would fail at line 1126: invalid input syntax for type integer: "12 шт"
        19 of 20000 records rejected, 19981 clean
        clean records:    orders.clean.csv
        rejected records: orders.rejects.csv
```

Код выхода 1. Что здесь видно:

- Номер `line` — физическая строка файла, которую можно открыть в редакторе. Записи с переводом строки внутри кавычек занимают несколько строк, и счёт это учитывает.
- Дата `20.03.2024` не прошла, потому что DateStyle по умолчанию `ISO, MDY`, и 20 читается как месяц. Подсказка про datestyle — та же, что даёт PostgreSQL. Если сервер настроен на `ISO, DMY` (initdb часто выбирает DMY для русской локали), об этом нужно сказать явно:

  ```
  $ pg-copycheck --ddl orders.sql --header match --datestyle 'ISO, DMY' orders.csv | tail -2
  RESULT: COPY would fail at line 1126: invalid input syntax for type integer: "12 шт"
          17 of 20000 records rejected, 19983 clean
  ```

  Отклонённых стало 17 вместо 19: дата теперь проходит, а дубль ключа без `--check-unique` не ищется.
- `MSK` — допустимое для PostgreSQL сокращение часового пояса, но таблицы сокращений в проверке нет. Поэтому значение попадает в предупреждения как «не решено», а строка не отклоняется. Подробнее об этом ниже.

Файл `orders.rejects.csv` выглядит так (начало):

```
record,line,column,sqlstate,message,detail,raw
1001,1126,quantity,22P02,"invalid input syntax for type integer: ""12 шт""",,"1000,19454,new,12 шт,11235.67,RUB,ivanov652@example.com,t,2024-12-19 04:01:13+03,2024-12-21,70c02139-8c36-4c36-bf2f-c79b2189b1c0,""{""""source"""":""""web"""",""""items"""":3}"",17.27,Оставить у двери"
```

`orders.clean.csv` — те же байты без отклонённых записей, вместе со строкой заголовка. Повторная проверка находит в нём только то же предупреждение про `MSK`:

```
$ pg-copycheck --ddl orders.sql --header match --check-unique orders.clean.csv | tail -2
RESULT: no problems found that would stop COPY
        0 of 19981 records rejected, 19981 clean
```

### Маркер `\.`

Строка, состоящая только из `\.`, до PostgreSQL 18 означает конец данных и в CSV: COPY молча останавливается на ней и не читает остаток файла. Ошибки при этом нет.

```
$ printf 'id,name\n1,first\n\\.\n2,second\n3,third\n' > marker.csv
$ printf 'CREATE TABLE public.t (id integer NOT NULL, name text);\n' > t.sql
$ pg-copycheck --ddl t.sql --header true marker.csv
...
DATA LOSS: COPY would succeed but skip data
   RECORDS  SQLSTATE  COLUMN          PROBLEM
         1                            end-of-copy marker "\." ends the data early
                                        line 3: line is "\." alone; PostgreSQL 17 stops reading CSV here and silently ignores the rest of the file

RESULT: COPY would succeed but skip 2 records after an end-of-copy marker
        1 of 4 records rejected, 3 clean
```

С `--pg-version 18` эта строка читается как данные: в этом примере она даёт ошибку типа в `id`, и остаётся предупреждение, что psql при чтении из STDIN по-прежнему считает её концом данных.

### Флаги

| Флаг | Что делает |
|---|---|
| `--ddl FILE` | схема из `pg_dump --schema-only` (обязателен) |
| `--table NAME` | таблица, `name` или `schema.name`; можно не указывать, если в схеме одна таблица |
| `--columns a,b,c` | список колонок COPY; по умолчанию все, кроме генерируемых |
| `--delimiter`, `--quote`, `--escape`, `--null` | параметры COPY; `--delimiter '\t'` для TSV |
| `--header false\|true\|match` | HEADER |
| `--force-null`, `--force-not-null` | FORCE_NULL и FORCE_NOT_NULL, список колонок или `*` |
| `--datestyle 'ISO, DMY'` | DateStyle сервера; значение имеет только порядок полей |
| `--pg-version 17` | версия сервера, от 14 до 18 |
| `--check-unique` | искать дубли PRIMARY KEY и UNIQUE внутри файла |
| `--clean FILE`, `--rejects FILE` | куда писать прошедшие и отклонённые записи |
| `--report text\|json` | формат отчёта |
| `--workers N` | число проверяющих горутин, по умолчанию по числу ядер |

Вместо файла можно передать `-`, тогда данные читаются со стандартного ввода. Флаги можно писать и до, и после имени файла.

Коды выхода: `0` — ничего, что остановит COPY или потеряет данные; `1` — COPY упадёт или пропустит данные; `2` — ошибка в аргументах, нечитаемый файл или команда COPY, которую сервер не примет (например, несуществующая колонка в `--columns`).

## Как проверить самому на PostgreSQL

На машине, где писался этот README, PostgreSQL нет, поэтому команды ниже здесь не запускались. Проверить вывод инструмента можно так:

```
createdb copycheck_demo
psql -d copycheck_demo -f orders.sql
psql -d copycheck_demo -c "\copy orders FROM 'orders.csv' WITH (FORMAT csv, HEADER match)"
psql -d copycheck_demo -c "\copy orders FROM 'orders.clean.csv' WITH (FORMAT csv, HEADER match)"
```

Первая загрузка должна упасть на `line 1126` с `invalid input syntax for type integer: "12 шт"`. Для однострочных записей номер в `CONTEXT: COPY orders, line N` совпадает с номером из отчёта, для многострочных PostgreSQL называет последнюю строку записи. Вторая загрузка должна пройти и вставить 19981 строку. Для отдельных значений можно использовать `SELECT * FROM pg_input_error_info('20.03.2024 13:04', 'timestamptz');` (PostgreSQL 16+): эта функция возвращает те же сообщения, что и COPY.

## Как это работает

**Чтение.** В PostgreSQL разбор CSV при COPY делится на два шага: `CopyReadLineText` ищет конец записи, `CopyReadAttributesCSV` режет запись на поля (`src/backend/commands/copyfromparse.c`). Оба шага перенесены с сохранением поведения:

- кавычка открывает quoted-секцию даже посреди поля;
- стиль перевода строки (LF, CR или CRLF) фиксируется по первой строке, и чужой CR или LF вне кавычек — ошибка;
- NULL — это только поле без кавычек, равное строке NULL, а `""` — пустая строка;
- `\.` обрабатывается в зависимости от версии.

Конец записи ищется последовательно, остальное делается параллельно. Последовательная часть прыгает по файлу через `bytes.IndexByte` к ближайшей кавычке, CR или LF, а не смотрит каждый байт. На тестовом файле это подняло её скорость примерно с 330 до 610 МБ/с (бенчмарк `BenchmarkReader`). Fuzz-тест проверяет главное свойство: записи в сумме дают ровно исходные байты, поэтому `clean` и `rejects` вместе всегда покрывают весь файл.

**Конвейер.** Чтение → пакеты (до 1024 записей или 1 МиБ, все байты пакета в одном буфере) → N воркеров → сборщик, который восстанавливает исходный порядок по номеру пакета → `clean`, `rejects` и сводка. Число пакетов в работе ограничено семафором (2 × воркеры + 2), поэтому память не зависит от размера файла. Если запись результата тормозит, чтение входа останавливается, и это проверяется тестом. Вывод побайтно одинаков при 1 и 8 воркерах, это тоже тест. Дубли ключей, маркер `\.` и порядок вывода обрабатываются в сборщике, который видит записи по порядку.

**Проверка значений.** Для каждого поддержанного типа перенесена функция ввода из `src/backend/utils/adt`: `numutils.c`, `numeric.c`, `float.c`, `bool.c`, `varchar.c`, `uuid.c`, `enum.c`, `datetime.c`, а также `src/common/jsonapi.c`. Сохранены:

- тексты сообщений и SQLSTATE;
- порядок ошибок: ошибки типов раньше NOT NULL, а неверная кодировка раньше всего;
- различия версий: подчёркивания и `0x`/`0o`/`0b` в числах с 16-й, `\.` в CSV с 18-й, HEADER MATCH с 15-й.

Ответ у проверки тройной: принято, отклонено или «не знаю». «Не знаю» означает, что значение вне перенесённых правил (названия месяцев, часовые пояса по имени, `interval`, массивы и т. п.). Такое значение попадает в предупреждения, а строка не отклоняется и идёт в `clean`: лучше честно не решить, чем угадать. Ожидаемые ответы лежат в `internal/pginput/testdata/corpus.tsv`, 183 случая. У каждого есть ссылка на regression-вывод (`src/test/regress/expected/*.out`) или исходник, откуда взят ответ. Тест требует точного совпадения вердикта и, где он указан, текста ошибки, и допускает «не знаю» только там, где корпус это разрешает.

**Уникальность.** С `--check-unique` для каждого ключа берётся каноническая форма значения в смысле равенства PostgreSQL: `1.0` и `1.00` для numeric, ` 17` и `0x11` для integer, uuid в любом регистре, `char(n)` без хвостовых пробелов, timestamptz, приведённый к UTC. От неё считается 128-битный хеш FNV-1a. Сборщик хранит хеши в map, поэтому дубль ищется только среди уже принятых строк, и первая из одинаковых остаётся в `clean`. Если каноническую форму без сервера вычислить нельзя (timestamptz без явного пояса зависит от TimeZone сессии), ключ не проверяется, и это видно в отчёте.

## Цифры

Измерено на i5-12400F (6 ядер, 12 потоков), 16 ГБ RAM, Windows 11, Go 1.27.3, на файлах из `genfixture`. Время — медиана трёх запусков, это время, которое печатает сам инструмент. Память — пиковый working set процесса.

| Файл | Воркеры | Время | Пик памяти |
|---|---|---|---|
| 20 000 записей, 3.5 МиБ | 12 | 0.02 с | 13–16 МиБ |
| 200 000 записей, 35 МиБ | 12 | 0.12 с | 20 МиБ |
| 2 000 000 записей, 354 МиБ | 1 | 2.3 с | 15 МиБ |
| 2 000 000 записей, 354 МиБ | 4 | 1.1 с | 19–20 МиБ |
| 2 000 000 записей, 354 МиБ | 12 | 1.0 с | 20–22 МиБ |
| 2 000 000 записей, 354 МиБ, `--check-unique` | 12 | 1.1 с | 290–315 МиБ |

От машины не зависит вот что. Память без `--check-unique` не растёт с размером файла: она ограничена числом пакетов в работе. С `--check-unique` память растёт линейно с числом строк: здесь два ключа на 2 млн строк, то есть около 70 байт на строку на ключ. Ускорение от воркеров упирается в последовательный поиск границ записей: после 4 воркеров на этой машине прирост небольшой.

## Ограничения

- Только `FORMAT csv`. Текстовый формат COPY (с `\N` и обратными слэшами) и binary не поддерживаются. TSV проверяется как CSV с `--delimiter '\t'`.
- Только UTF-8, как у сервера с `client_encoding = UTF8`. Опция `ENCODING` не моделируется. Есть только подсказка, если байты похожи на Windows-1251.
- Проверяемые типы: `smallint`, `integer`, `bigint`, `numeric(p,s)`, `real`, `double precision`, `boolean`, `text`, `varchar(n)`, `char(n)`, `date`, `timestamp`, `timestamptz`, `uuid`, `json`, `jsonb`, enum. Всё остальное (`time`, `interval`, `bytea`, `inet`, массивы, домены, составные типы) отмечается как непроверенное.
- Даты и время решаются только в числовых формах (`2024-01-05`, `05.01.2024`, `1/5/24` с учётом DateStyle, время, смещение `+03`, `+03:00`, `Z`, `UTC`, `GMT`) и для специальных слов (`epoch`, `infinity`, `now`, ...). Названия месяцев, BC, юлианские дни и пояса по имени дают «не знаю». Байты вне ASCII в дате считаются ошибкой, как в локалях C и UTF-8 на Linux.
- Не проверяются CHECK, внешние ключи, триггеры и уникальность относительно строк, уже лежащих в таблице. Предполагается детерминированная collation.
- Из DDL читается только то, что выдаёт `pg_dump --schema-only`, и простой рукописный `CREATE TABLE`. Таблицы с `INHERITS`, `PARTITION OF` и `LIKE` инструмент отказывается проверять, потому что их колонки из файла не видны.
- Пока нет: чтения схемы из работающей базы (`--dsn`), проверки «не знаю»-значений на настоящем сервере через `pg_input_error_info` (`--verify-dsn`) и дифференциальных тестов против живого PostgreSQL. Сейчас соответствие PostgreSQL проверено по исходникам и regression-выводам, а не прогоном на сервере.

## Лицензия

MIT, см. [LICENSE](LICENSE).
