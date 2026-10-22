-- Semantic section search, component "semantic_index" v1 (docs/SEMANTIC-SEARCH.md).
-- Chunk vectors of an article are derived from its text, so they get exactly the
-- visibility of the article: every policy delegates to the row-level security of
-- public.articles (the subquery runs with the caller's policies, including MCP
-- project scopes). There is no UPDATE policy: a changed text gets a new key.
CREATE TABLE article_embeddings (
    article_id text NOT NULL REFERENCES articles(id),
    model text NOT NULL CHECK (length(model) BETWEEN 1 AND 200),
    text_key text NOT NULL CHECK (text_key ~ '^[0-9a-f]{64}$'),
    dims integer NOT NULL CHECK (dims BETWEEN 1 AND 8192),
    vector bytea NOT NULL CHECK (octet_length(vector) = dims * 4),
    created_at text NOT NULL,
    PRIMARY KEY (article_id, model, text_key)
);
ALTER TABLE article_embeddings ENABLE ROW LEVEL SECURITY;
ALTER TABLE article_embeddings FORCE ROW LEVEL SECURITY;
CREATE POLICY article_embeddings_read ON article_embeddings FOR SELECT
USING (EXISTS (SELECT 1 FROM public.articles a WHERE a.id = article_embeddings.article_id));
CREATE POLICY article_embeddings_insert ON article_embeddings FOR INSERT
WITH CHECK (EXISTS (SELECT 1 FROM public.articles a WHERE a.id = article_embeddings.article_id));
CREATE POLICY article_embeddings_delete ON article_embeddings FOR DELETE
USING (EXISTS (SELECT 1 FROM public.articles a WHERE a.id = article_embeddings.article_id));
