CREATE TABLE `sessions` (
  `uuid` char(36) NOT NULL,
  `user_id` bigint unsigned NOT NULL,
  `created_at` datetime NOT NULL,
  PRIMARY KEY (`uuid`),
  KEY `ix_user` (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci ROW_FORMAT=DYNAMIC
