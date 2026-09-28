-- Схема квалификатора одной миграцией. Кейс - проекция, decisions и
-- case_events - журнал, outbox - действия наружу в транзакции решения.

CREATE TABLE IF NOT EXISTS leads (
    id         bigserial PRIMARY KEY,
    name       text NOT NULL DEFAULT '',
    peer_ref   text NOT NULL UNIQUE,
    crm_ref    text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS cases (
    id              bigserial PRIMARY KEY,
    lead_id         bigint NOT NULL REFERENCES leads(id),
    status          text NOT NULL CHECK (status IN ('collecting', 'ready_to_decide', 'meeting_scheduled',
                                                    'rejected', 'stopped', 'manual_review', 'refused')),
    facts           jsonb NOT NULL DEFAULT '{}',
    decision_reason text NOT NULL DEFAULT '',
    attempts        integer NOT NULL DEFAULT 0,
    next_action_at  timestamptz NOT NULL DEFAULT now(),
    last_error_kind text NOT NULL DEFAULT '',
    last_error      text NOT NULL DEFAULT '',
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);

-- Один кейс на лида: повторный вход лида продолжает тот же разговор.
CREATE UNIQUE INDEX IF NOT EXISTS cases_lead_idx ON cases (lead_id);

CREATE TABLE IF NOT EXISTS messages (
    id           bigserial PRIMARY KEY,
    case_id      bigint NOT NULL REFERENCES cases(id),
    direction    text NOT NULL CHECK (direction IN ('in', 'out')),
    text         text NOT NULL,
    external_id  text NOT NULL DEFAULT '',
    -- processed_at - ход ответил на эту реплику лида; у исходящих пусто.
    processed_at timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS messages_case_idx ON messages (case_id, id);
CREATE UNIQUE INDEX IF NOT EXISTS messages_external_idx ON messages (case_id, direction, external_id)
    WHERE external_id <> '';

CREATE TABLE IF NOT EXISTS decisions (
    id           bigserial PRIMARY KEY,
    case_id      bigint NOT NULL REFERENCES cases(id),
    source       text NOT NULL,
    status       text NOT NULL,
    next_action  text NOT NULL,
    reason       text NOT NULL DEFAULT '',
    facts        jsonb NOT NULL DEFAULT '{}',
    message      text NOT NULL DEFAULT '',
    raw_response jsonb NOT NULL DEFAULT '{}',
    created_at   timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS case_events (
    id         bigserial PRIMARY KEY,
    case_id    bigint NOT NULL REFERENCES cases(id),
    event_type text NOT NULL,
    payload    jsonb NOT NULL DEFAULT '{}',
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS case_events_case_idx ON case_events (case_id, id);

CREATE TABLE IF NOT EXISTS offered_slots (
    case_id    bigint NOT NULL REFERENCES cases(id),
    manager_id text NOT NULL,
    starts_at  timestamptz NOT NULL,
    ends_at    timestamptz NOT NULL,
    expires_at timestamptz NOT NULL,
    PRIMARY KEY (case_id, starts_at)
);

CREATE TABLE IF NOT EXISTS meetings (
    id         bigserial PRIMARY KEY,
    case_id    bigint NOT NULL REFERENCES cases(id),
    manager_id text NOT NULL,
    starts_at  timestamptz NOT NULL,
    ends_at    timestamptz NOT NULL,
    status     text NOT NULL CHECK (status IN ('scheduled', 'cancelled')),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- Двойную бронь держит база, а не подбор окон: два лида на одно окно
-- менеджера не пройдут, сколько бы ходов ни шло параллельно.
CREATE UNIQUE INDEX IF NOT EXISTS meetings_slot_idx ON meetings (manager_id, starts_at)
    WHERE status = 'scheduled';
CREATE UNIQUE INDEX IF NOT EXISTS meetings_case_idx ON meetings (case_id)
    WHERE status = 'scheduled';

CREATE TABLE IF NOT EXISTS outbox (
    id              bigserial PRIMARY KEY,
    case_id         bigint NOT NULL REFERENCES cases(id),
    kind            text NOT NULL CHECK (kind IN ('message', 'crm_note', 'crm_task')),
    purpose         text NOT NULL,
    cause_key       text NOT NULL,
    text            text NOT NULL,
    manager_id      text NOT NULL DEFAULT '',
    status          text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'sending', 'sent', 'failed', 'cancelled')),
    attempts        integer NOT NULL DEFAULT 0,
    failures        integer NOT NULL DEFAULT 0,
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    external_id     text NOT NULL DEFAULT '',
    last_error      text NOT NULL DEFAULT '',
    created_at      timestamptz NOT NULL DEFAULT now(),
    sent_at         timestamptz
);

-- Повтор решения на тот же вход не ставит вторую строку.
CREATE UNIQUE INDEX IF NOT EXISTS outbox_cause_idx ON outbox (case_id, kind, purpose, cause_key);
CREATE INDEX IF NOT EXISTS outbox_pending_idx ON outbox (next_attempt_at, id) WHERE status IN ('pending', 'sending');
