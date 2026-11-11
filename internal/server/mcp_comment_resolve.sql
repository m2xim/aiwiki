-- Agents resolve comments through MCP. Under an MCP scope a comment UPDATE is allowed
-- when the key has a write element, and only the "resolved" column may change (a
-- trigger enforces that, since a WITH CHECK cannot see OLD). Authorship of a service
-- key is the svc_ actor id, while app.user_id is the sponsor: the "own comment" test
-- also accepts the session actor (app.actor_id, set by the server next to app.user_id),
-- but only for a live service account owned by the session user.
CREATE OR REPLACE FUNCTION public.corporate_comment_author_ok(author text)
RETURNS boolean LANGUAGE sql STABLE SECURITY DEFINER
SET search_path=pg_catalog,public AS $fn$
 SELECT coalesce(author IS NOT NULL AND author<>'' AND (
   author=current_setting('app.user_id',true)
   OR (author=current_setting('app.actor_id',true) AND author LIKE 'svc\_%'
       AND EXISTS (SELECT 1 FROM public.mcp_service_accounts s
                    WHERE s.id=author AND s.owner_id=current_setting('app.user_id',true) AND s.disabled=0))),false)
$fn$;
REVOKE ALL ON FUNCTION public.corporate_comment_author_ok(text) FROM PUBLIC;

ALTER POLICY corporate_comments_insert ON comments WITH CHECK (
    EXISTS (SELECT 1 FROM public.articles a WHERE a.id=comments.article_id
      AND public.corporate_role_rank(public.corporate_effective_role(current_setting('app.user_id',true),a.space_id,a.project_id)) >= 2
      AND (public.corporate_comment_author_ok(comments.author)
        OR public.corporate_role_rank(public.corporate_effective_role(current_setting('app.user_id',true),a.space_id,a.project_id)) >= 3)
      AND public.corporate_container_writable(a.space_id,a.project_id)));
ALTER POLICY corporate_comments_update ON comments USING (
    EXISTS (SELECT 1 FROM public.articles a WHERE a.id=comments.article_id
      AND public.corporate_role_rank(public.corporate_effective_role(current_setting('app.user_id',true),a.space_id,a.project_id)) >= 2
      AND (public.corporate_comment_author_ok(comments.author)
        OR public.corporate_role_rank(public.corporate_effective_role(current_setting('app.user_id',true),a.space_id,a.project_id)) >= 3)
      AND public.corporate_container_writable(a.space_id,a.project_id)))
WITH CHECK (EXISTS (SELECT 1 FROM public.articles a WHERE a.id=comments.article_id
      AND public.corporate_role_rank(public.corporate_effective_role(current_setting('app.user_id',true),a.space_id,a.project_id)) >= 2
      AND (public.corporate_comment_author_ok(comments.author)
        OR public.corporate_role_rank(public.corporate_effective_role(current_setting('app.user_id',true),a.space_id,a.project_id)) >= 3)
      AND public.corporate_container_writable(a.space_id,a.project_id)));

ALTER POLICY mcp_scope_comments_update ON comments
  USING (public.corporate_mcp_content_write_allowed())
  WITH CHECK (public.corporate_mcp_content_write_allowed());

CREATE OR REPLACE FUNCTION public.corporate_comments_mcp_resolve_only() RETURNS trigger
LANGUAGE plpgsql SET search_path=pg_catalog,public AS $fn$
BEGIN
 IF public.corporate_mcp_scope_active() AND
    ROW(NEW.id,NEW.article_id,NEW.anchor,NEW.body,NEW.author,NEW.created_at)
    IS DISTINCT FROM ROW(OLD.id,OLD.article_id,OLD.anchor,OLD.body,OLD.author,OLD.created_at) THEN
  RAISE EXCEPTION 'MCP sessions may only change the resolved flag of a comment';
 END IF;
 RETURN NEW;
END
$fn$;
REVOKE ALL ON FUNCTION public.corporate_comments_mcp_resolve_only() FROM PUBLIC;
DROP TRIGGER IF EXISTS comments_mcp_resolve_only ON comments;
CREATE TRIGGER comments_mcp_resolve_only BEFORE UPDATE ON comments
 FOR EACH ROW EXECUTE FUNCTION public.corporate_comments_mcp_resolve_only();
