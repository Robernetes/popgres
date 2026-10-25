package migrations

// popgresStrftimeFunctions creates the popgres_strftime SQL functions used by the
// strftime() filter function (an emulation of the SQLite strftime with its
// time values, modifiers and format substitutions).
//
// The functions run with the UTC time zone (the same as SQLite) and return
// NULL for invalid time values, modifiers or format substitutions.
const popgresStrftimeFunctions = `
	-- popgres_strftime_apply applies the SQLite date modifiers to the provided timestamp.
	CREATE OR REPLACE FUNCTION popgres_strftime_apply(ts TIMESTAMPTZ, mods TEXT[]) RETURNS TIMESTAMPTZ
	LANGUAGE plpgsql STABLE PARALLEL SAFE
	SET timezone TO 'UTC'
	AS $$
	DECLARE
		m TEXT;
		parts TEXT[];
		n DOUBLE PRECISION;
		target INTEGER;
	BEGIN
		IF ts IS NULL THEN
			RETURN NULL;
		END IF;

		FOREACH m IN ARRAY COALESCE(mods, '{}'::TEXT[]) LOOP
			m := lower(btrim(m));

			IF m IN ('unixepoch', 'julianday', 'auto', 'utc', 'localtime', 'subsec', 'subsecond') THEN
				CONTINUE;
			END IF;

			IF m = 'start of day' THEN
				ts := date_trunc('day', ts);
			ELSIF m = 'start of month' THEN
				ts := date_trunc('month', ts);
			ELSIF m = 'start of year' THEN
				ts := date_trunc('year', ts);
			ELSIF m ~ '^weekday [0-6]$' THEN
				target := substr(m, 9, 1)::INTEGER;
				ts := ts + (((target - extract(dow FROM ts)::INTEGER) + 7) % 7) * INTERVAL '1 day';
			ELSIF m ~ '^[+-]?\s*[0-9]+(\.[0-9]+)?\s+(day|hour|minute|second|month|year)s?$' THEN
				parts := regexp_match(m, '^([+-]?)\s*([0-9]+(?:\.[0-9]+)?)\s+(day|hour|minute|second|month|year)s?$');
				n := parts[2]::DOUBLE PRECISION;
				IF parts[1] = '-' THEN
					n := -n;
				END IF;
				IF parts[3] IN ('month', 'year') THEN
					-- whole months/years only (the same as the SQLite integer part)
					ts := ts + (trunc(n)::INTEGER || ' ' || parts[3])::INTERVAL;
				ELSE
					ts := ts + (n || ' ' || parts[3])::INTERVAL;
				END IF;
			ELSIF m ~ '^[+-][0-9]{1,2}:[0-9]{2}(:[0-9]{2}(\.[0-9]+)?)?$' THEN
				ts := ts + (CASE WHEN left(m, 1) = '-' THEN -1 ELSE 1 END) * substr(m, 2)::INTERVAL;
			ELSE
				RETURN NULL; -- unknown modifier
			END IF;
		END LOOP;

		RETURN ts;
	END
	$$;

	-- popgres_strftime_value parses a SQLite time value (date string, "now" or number).
	CREATE OR REPLACE FUNCTION popgres_strftime_value(v TEXT, mods TEXT[]) RETURNS TIMESTAMPTZ
	LANGUAGE plpgsql STABLE PARALLEL SAFE
	SET timezone TO 'UTC'
	AS $$
	DECLARE
		num DOUBLE PRECISION;
		lmods TEXT[];
	BEGIN
		IF v IS NULL THEN
			RETURN NULL;
		END IF;

		v := btrim(v);

		IF lower(v) = 'now' THEN
			RETURN clock_timestamp();
		END IF;

		IF v ~ '^[+-]?[0-9]+(\.[0-9]+)?$' THEN
			num := v::DOUBLE PRECISION;
			SELECT array_agg(lower(btrim(x))) INTO lmods FROM unnest(COALESCE(mods, '{}'::TEXT[])) AS x;

			IF 'unixepoch' = ANY(COALESCE(lmods, '{}'::TEXT[]))
				OR ('auto' = ANY(COALESCE(lmods, '{}'::TEXT[])) AND (num < 0 OR num > 5373484.499999)) THEN
				RETURN to_timestamp(num);
			END IF;

			-- julian day number
			RETURN to_timestamp((num - 2440587.5) * 86400);
		END IF;

		-- "HH:MM[:SS[.SSS]]" without a date is relative to 2000-01-01
		IF v ~ '^[0-9]{2}:[0-9]{2}(:[0-9]{2}(\.[0-9]+)?)?$' THEN
			v := '2000-01-01 ' || v;
		END IF;

		BEGIN
			-- the values without time zone are UTC (see the function timezone setting)
			RETURN v::TIMESTAMPTZ;
		EXCEPTION WHEN others THEN
			RETURN NULL;
		END;
	END
	$$;

	-- popgres_strftime_format formats the timestamp with the SQLite strftime substitutions.
	CREATE OR REPLACE FUNCTION popgres_strftime_format(fmt TEXT, ts TIMESTAMPTZ) RETURNS TEXT
	LANGUAGE plpgsql STABLE PARALLEL SAFE
	SET timezone TO 'UTC'
	AS $$
	DECLARE
		result TEXT := '';
		i INTEGER := 1;
		c TEXT;
		yday INTEGER;
		wday INTEGER;
	BEGIN
		IF fmt IS NULL OR ts IS NULL THEN
			RETURN NULL;
		END IF;

		yday := extract(doy FROM ts)::INTEGER - 1;
		wday := extract(dow FROM ts)::INTEGER;

		WHILE i <= length(fmt) LOOP
			c := substr(fmt, i, 1);

			IF c <> '%' THEN
				result := result || c;
				i := i + 1;
				CONTINUE;
			END IF;

			c := substr(fmt, i + 1, 1);
			i := i + 2;

			CASE c
				WHEN 'd' THEN result := result || to_char(ts, 'DD');
				WHEN 'e' THEN result := result || lpad(extract(day FROM ts)::INTEGER::TEXT, 2, ' ');
				WHEN 'f' THEN result := result || to_char(ts, 'SS.MS');
				WHEN 'F' THEN result := result || to_char(ts, 'YYYY-MM-DD');
				WHEN 'G' THEN result := result || to_char(ts, 'IYYY');
				WHEN 'g' THEN result := result || to_char(ts, 'IY');
				WHEN 'H' THEN result := result || to_char(ts, 'HH24');
				WHEN 'I' THEN result := result || to_char(ts, 'HH12');
				WHEN 'j' THEN result := result || to_char(ts, 'DDD');
				WHEN 'J' THEN result := result || ((extract(epoch FROM ts) / 86400) + 2440587.5)::TEXT;
				WHEN 'k' THEN result := result || lpad(extract(hour FROM ts)::INTEGER::TEXT, 2, ' ');
				WHEN 'l' THEN result := result || lpad(ltrim(to_char(ts, 'HH12'), '0'), 2, ' ');
				WHEN 'm' THEN result := result || to_char(ts, 'MM');
				WHEN 'M' THEN result := result || to_char(ts, 'MI');
				WHEN 'p' THEN result := result || to_char(ts, 'AM');
				WHEN 'P' THEN result := result || to_char(ts, 'am');
				WHEN 'R' THEN result := result || to_char(ts, 'HH24:MI');
				WHEN 's' THEN result := result || floor(extract(epoch FROM ts))::BIGINT::TEXT;
				WHEN 'S' THEN result := result || to_char(ts, 'SS');
				WHEN 'T' THEN result := result || to_char(ts, 'HH24:MI:SS');
				WHEN 'u' THEN result := result || to_char(ts, 'ID');
				WHEN 'U' THEN result := result || lpad(((yday + 7 - wday) / 7)::TEXT, 2, '0');
				WHEN 'V' THEN result := result || to_char(ts, 'IW');
				WHEN 'w' THEN result := result || wday::TEXT;
				WHEN 'W' THEN result := result || lpad(((yday + 7 - ((wday + 6) % 7)) / 7)::TEXT, 2, '0');
				WHEN 'Y' THEN result := result || to_char(ts, 'YYYY');
				WHEN '%' THEN result := result || '%';
				ELSE RETURN NULL; -- unknown substitution
			END CASE;
		END LOOP;

		RETURN result;
	END
	$$;

	-- popgres_strftime(format) formats the current time.
	CREATE OR REPLACE FUNCTION popgres_strftime(fmt TEXT) RETURNS TEXT
	LANGUAGE sql STABLE PARALLEL SAFE AS $$
		SELECT popgres_strftime_format(fmt, clock_timestamp())
	$$;

	-- popgres_strftime(format, timeValue [, modifiers...]) for text, date and number time values.
	CREATE OR REPLACE FUNCTION popgres_strftime(fmt TEXT, v TEXT, VARIADIC mods TEXT[]) RETURNS TEXT
	LANGUAGE sql STABLE PARALLEL SAFE AS $$
		SELECT popgres_strftime_format(fmt, popgres_strftime_apply(popgres_strftime_value(v, mods), mods))
	$$;

	CREATE OR REPLACE FUNCTION popgres_strftime(fmt TEXT, v TEXT) RETURNS TEXT
	LANGUAGE sql STABLE PARALLEL SAFE AS $$
		SELECT popgres_strftime_format(fmt, popgres_strftime_value(v, '{}'::TEXT[]))
	$$;

	CREATE OR REPLACE FUNCTION popgres_strftime(fmt TEXT, v TIMESTAMPTZ, VARIADIC mods TEXT[]) RETURNS TEXT
	LANGUAGE sql STABLE PARALLEL SAFE AS $$
		SELECT popgres_strftime_format(fmt, popgres_strftime_apply(v, mods))
	$$;

	CREATE OR REPLACE FUNCTION popgres_strftime(fmt TEXT, v TIMESTAMPTZ) RETURNS TEXT
	LANGUAGE sql STABLE PARALLEL SAFE AS $$
		SELECT popgres_strftime_format(fmt, v)
	$$;

	CREATE OR REPLACE FUNCTION popgres_strftime(fmt TEXT, v DOUBLE PRECISION, VARIADIC mods TEXT[]) RETURNS TEXT
	LANGUAGE sql STABLE PARALLEL SAFE AS $$
		SELECT popgres_strftime(fmt, v::TEXT, VARIADIC mods)
	$$;

	CREATE OR REPLACE FUNCTION popgres_strftime(fmt TEXT, v DOUBLE PRECISION) RETURNS TEXT
	LANGUAGE sql STABLE PARALLEL SAFE AS $$
		SELECT popgres_strftime(fmt, v::TEXT)
	$$;

	-- popgres_to_number converts a json number to double precision (NULL for any other value).
	CREATE OR REPLACE FUNCTION popgres_to_number(v JSONB) RETURNS DOUBLE PRECISION
	LANGUAGE plpgsql IMMUTABLE PARALLEL SAFE AS $$
	BEGIN
		IF jsonb_typeof(v) = 'number' THEN
			RETURN (v #>> '{}')::DOUBLE PRECISION;
		END IF;
		RETURN NULL;
	END
	$$;
`

const dropPopgresStrftimeFunctions = `
	DROP FUNCTION IF EXISTS popgres_to_number(JSONB);
	DROP FUNCTION IF EXISTS
		popgres_strftime(TEXT),
		popgres_strftime(TEXT, TEXT), popgres_strftime(TEXT, TEXT, VARIADIC TEXT[]),
		popgres_strftime(TEXT, TIMESTAMPTZ), popgres_strftime(TEXT, TIMESTAMPTZ, VARIADIC TEXT[]),
		popgres_strftime(TEXT, DOUBLE PRECISION), popgres_strftime(TEXT, DOUBLE PRECISION, VARIADIC TEXT[]);
	DROP FUNCTION IF EXISTS popgres_strftime_format(TEXT, TIMESTAMPTZ), popgres_strftime_value(TEXT, TEXT[]), popgres_strftime_apply(TIMESTAMPTZ, TEXT[]);
`
