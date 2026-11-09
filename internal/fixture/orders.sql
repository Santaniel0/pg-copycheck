--
-- Schema for the generated demo data, in the shape pg_dump --schema-only writes.
--

CREATE TYPE public.order_status AS ENUM (
    'new',
    'paid',
    'shipped',
    'cancelled'
);

CREATE TABLE public.orders (
    id bigint NOT NULL,
    customer_id integer NOT NULL,
    status public.order_status DEFAULT 'new'::public.order_status NOT NULL,
    quantity integer NOT NULL,
    amount numeric(12,2) NOT NULL,
    currency character(3) DEFAULT 'RUB'::bpchar NOT NULL,
    email character varying(64),
    paid boolean DEFAULT false NOT NULL,
    created_at timestamp with time zone NOT NULL,
    shipped_on date,
    external_id uuid,
    meta jsonb,
    weight_kg real,
    comment text,
    total numeric(14,2) GENERATED ALWAYS AS ((amount * 1.2)) STORED
);

ALTER TABLE ONLY public.orders
    ADD CONSTRAINT orders_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.orders
    ADD CONSTRAINT orders_external_id_key UNIQUE (external_id);
