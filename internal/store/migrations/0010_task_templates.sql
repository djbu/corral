-- M9 persists the selected operator-authorized template for DAG tasks.
-- The row contains only the name; executable configuration and permissions
-- remain outside repo-controlled DAG input and outside the database.
ALTER TABLE tasks ADD COLUMN template_name TEXT;
