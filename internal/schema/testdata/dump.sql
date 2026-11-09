--
-- PostgreSQL database dump
--

\restrict 9fQwJ1pZc3sQ8yX5mH2kD7vN4tR6bL0e

-- Dumped from database version 17.6
-- Dumped by pg_dump version 17.6

SET statement_timeout = 0;
SET lock_timeout = 0;
SET client_encoding = 'UTF8';
SET standard_conforming_strings = on;
SELECT pg_catalog.set_config('search_path', '', false);
SET check_function_bodies = false;

--
-- Name: billing; Type: SCHEMA; Schema: -; Owner: shop
--

CREATE SCHEMA billing;

--
-- Name: order_status; Type: TYPE; Schema: public; Owner: shop
--

CREATE TYPE public.order_status AS ENUM (
    'new',
    'paid',
    'shipped',
    'cancelled'
);


ALTER TYPE public.order_status OWNER TO shop;

--
-- Name: touch(); Type: FUNCTION; Schema: public; Owner: shop
--

CREATE FUNCTION public.touch() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    NEW.updated_at := now(); -- semicolons inside the body must not split statements
    RETURN NEW;
END;
$$;

SET default_tablespace = '';

SET default_table_access_method = heap;

--
-- Name: orders; Type: TABLE; Schema: public; Owner: shop
--

CREATE TABLE public.orders (
    id bigint NOT NULL,
    customer_id integer NOT NULL,
    status public.order_status DEFAULT 'new'::public.order_status NOT NULL,
    amount numeric(12,2) NOT NULL,
    currency character(3) DEFAULT 'RUB'::bpchar NOT NULL,
    email character varying(64) DEFAULT NULL::character varying,
    "Note" text COLLATE pg_catalog."C",
    paid boolean DEFAULT false NOT NULL,
    created_at timestamp(3) with time zone DEFAULT now() NOT NULL,
    shipped_on date,
    external_id uuid,
    tags text[],
    "char" "char",
    meta jsonb,
    total numeric(14,2) GENERATED ALWAYS AS ((amount * 1.2)) STORED,
    CONSTRAINT orders_amount_check CHECK ((amount >= (0)::numeric))
);


ALTER TABLE public.orders OWNER TO shop;

--
-- Name: invoices; Type: TABLE; Schema: billing; Owner: shop
--

CREATE TABLE billing.invoices (
    id integer NOT NULL,
    order_id bigint,
    issued_on date NOT NULL
);

ALTER TABLE billing.invoices ALTER COLUMN id ADD GENERATED ALWAYS AS IDENTITY (
    SEQUENCE NAME billing.invoices_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1
);

CREATE TABLE public.orders_archive (
    archived_at timestamp without time zone
)
INHERITS (public.orders);

CREATE SEQUENCE public.orders_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

ALTER SEQUENCE public.orders_id_seq OWNED BY public.orders.id;

ALTER TABLE ONLY public.orders ALTER COLUMN id SET DEFAULT nextval('public.orders_id_seq'::regclass);

ALTER TABLE ONLY public.orders
    ADD CONSTRAINT orders_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.orders
    ADD CONSTRAINT orders_external_id_key UNIQUE (external_id);

CREATE UNIQUE INDEX orders_email_lower_idx ON public.orders USING btree (lower((email)::text));

CREATE UNIQUE INDEX orders_customer_paid_idx ON public.orders USING btree (customer_id) WHERE paid;

CREATE UNIQUE INDEX invoices_order_idx ON billing.invoices USING btree (order_id) NULLS NOT DISTINCT;

COMMENT ON TABLE public.orders IS 'Orders; one row per checkout';

\unrestrict 9fQwJ1pZc3sQ8yX5mH2kD7vN4tR6bL0e
