-- M9 persists only the selected user-authorized template name. Template
-- contents remain operator configuration, never repo-controlled database data.
ALTER TABLE sessions ADD COLUMN template_name TEXT;
