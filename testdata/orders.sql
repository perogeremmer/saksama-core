CREATE TABLE `orders` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `spot_id` bigint unsigned NOT NULL,
  `user_id` bigint unsigned NOT NULL,
  `status` varchar(32) COLLATE utf8mb4_0900_ai_ci NOT NULL DEFAULT 'pending',
  `amount` decimal(18,2) NOT NULL DEFAULT '0.00',
  `created_at` datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `paid_at` datetime DEFAULT NULL,
  `note` text COLLATE utf8mb4_0900_ai_ci,
  PRIMARY KEY (`id`),
  KEY `ix_spot` (`spot_id`),
  KEY `ix_user` (`user_id`),
  CONSTRAINT `fk_orders_spot` FOREIGN KEY (`spot_id`) REFERENCES `spots` (`id`) ON DELETE CASCADE ON UPDATE SET NULL,
  CONSTRAINT `fk_orders_user` FOREIGN KEY (`user_id`) REFERENCES `users` (`id`)
) ENGINE=InnoDB AUTO_INCREMENT=59000001 DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci ROW_FORMAT=DYNAMIC
