DEFAULT_REPORTS = [
    {
        "name": "daily_active_users",
        "dimensions": ["date"],
        "metrics": ["active1DayUsers"]
    },
    {
        "name": "weekly_active_users",
        "dimensions": ["date"],
        "metrics": ["active7DayUsers"]
    },
    {
        "name": "four_weekly_active_users",
        "dimensions": ["date"],
        "metrics": ["active28DayUsers"]
    },
    {
        "name": "devices",
        "dimensions": ["date", "deviceCategory", "operatingSystem", "browser"],
        "metrics": [
            "totalUsers",
            "newUsers",
            "sessions",
            "sessionsPerUser",
            "averageSessionDuration",
            "screenPageViews",
            "screenPageViewsPerSession",
            "bounceRate"
        ]
    },
    {
        "name": "locations",
        "dimensions": ["region", "country", "city", "date"],
        "metrics": [
            "totalUsers",
            "newUsers",
            "sessions",
            "sessionsPerUser",
            "averageSessionDuration",
            "screenPageViews",
            "screenPageViewsPerSession",
            "bounceRate"
        ]
    },
    {
        "name": "pages",
        "dimensions": ["date", "hostName", "pagePathPlusQueryString"],
        "metrics": ["screenPageViews", "bounceRate"]
    },
    {
        "name": "traffic_sources",
        "dimensions": ["date", "sessionSource", "sessionMedium"],
        "metrics": [
            "totalUsers",
            "newUsers",
            "sessions",
            "sessionsPerUser",
            "averageSessionDuration",
            "screenPageViews",
            "screenPageViewsPerSession",
            "bounceRate"
        ]
    },
    {
        "name": "website_overview",
        "dimensions": ["date"],
        "metrics": [
            "totalUsers",
            "newUsers",
            "sessions",
            "sessionsPerUser",
            "averageSessionDuration",
            "screenPageViews",
            "screenPageViewsPerSession",
            "bounceRate"
        ]
    }
]