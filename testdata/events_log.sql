CREATE TABLE `events_log` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `uuid` char(36) NOT NULL,
  `payload` text COLLATE utf8mb4_0900_ai_ci,
  `created_at` datetime NOT NULL,
  PRIMARY KEY (`id`),
  KEY `ix_created` (`created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci ROW_FORMAT=DYNAMIC
